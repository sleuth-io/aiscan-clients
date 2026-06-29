package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/auth"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/redact"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/syncplan"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/upload"
)

// Sync implements `aiscan sync`: the v1 sync contract. Per source it discovers
// the raw available span (earliest data … now), asks the server which spans are
// still needed (aiscanSyncPlan), and uploads each as evidence — an empty body
// for a needed span that holds no sessions, recording a confirmed-empty window.
// Unlike `run`, the server hands back the exact work-list; the client does no
// interval math of its own.
func Sync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	instance := fs.String("instance", defaultInstance, "aiscan instance URL to sync with")
	source := fs.String("source", "", "only sync these comma-separated sources (e.g. claude-cowork); empty = all")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Sync local AI-tool usage to the aiscan server over the spans it still")
		fmt.Fprintln(os.Stderr, "needs. Discovers what exists, asks the server what's missing, uploads only")
		fmt.Fprintln(os.Stderr, "that. Authorizes once via the device-code flow.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan sync [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--instance URL", 19)), "aiscan instance to sync with (default "+defaultInstance+")")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--source LIST", 19)), "only sync these comma-separated sources (e.g. "+knownSourceList(recipes)+"); empty = all")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	selected, err := selectRecipes(recipes, *source)
	if err != nil {
		return err
	}

	ctx := context.Background()
	prompt := devicePrompt(os.Stdout)
	token, err := auth.EnsureToken(ctx, *instance, prompt)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	for _, r := range selected {
		if err := syncSource(ctx, *instance, &token, r, prompt, os.Stdout); err != nil {
			return err
		}
	}
	return nil
}

// syncSource runs the discover → plan → upload loop for one source. A source not
// present on this machine (or without discovery) is skipped, not an error.
func syncSource(ctx context.Context, instance string, token *string, r capture.Recipe, prompt auth.Prompt, w io.Writer) error {
	if (r.Detect != nil && !r.Detect()) || r.Discover == nil {
		fmt.Fprintf(w, "%s %s — not detected, skipping\n", dim("sync:"), header(string(r.ID)))
		return nil
	}
	earliest, err := r.Discover(ctx)
	if err != nil {
		return fmt.Errorf("discover %s: %w", r.ID, err)
	}
	if earliest.IsZero() {
		fmt.Fprintf(w, "%s %s — no data, skipping\n", dim("sync:"), header(string(r.ID)))
		return nil
	}

	available := syncplan.Span{Start: earliest.UTC(), End: time.Now().UTC()}
	fmt.Fprintf(w, "%s %s — available %s\n", header("sync:"), header(string(r.ID)), accent(formatSpan(available)))

	needed, err := planWithRetry(ctx, instance, token, string(r.ID), prompt, []syncplan.Span{available})
	if err != nil {
		return fmt.Errorf("plan %s: %w", r.ID, err)
	}
	if len(needed) == 0 {
		fmt.Fprintf(w, "  %s up to date (no spans needed)\n", dim("·"))
		return nil
	}

	for _, span := range needed {
		if err := syncSpan(ctx, instance, token, r, span, prompt, w); err != nil {
			return err
		}
	}
	return nil
}

// syncSpan captures the artifacts whose mtime falls within span, redacts them,
// and uploads them as evidence for that declared window. A span with no sessions
// is still posted — with an empty body — so the server records the window as
// confirmed-empty and never asks for it again. A span whose compressed body
// exceeds the size cap is split into parts, all declaring the same window.
func syncSpan(ctx context.Context, instance string, token *string, r capture.Recipe, span syncplan.Span, prompt auth.Prompt, w io.Writer) error {
	arts, errs := capture.Run(ctx, []capture.Recipe{r}, capture.Options{Since: span.Start, Until: span.End})
	for _, e := range errs {
		fmt.Fprintf(w, "%s %v\n", warn("warning:"), e)
	}

	// Redaction is the only gate before the wire; it always runs here.
	redacted, _ := redact.Redact(arts)
	if len(redacted) == 0 {
		res, err := uploadEvidence(ctx, instance, token, r.ID, span, nil, 0, prompt)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %s %s — empty → %s\n", success("synced"), dim(formatSpan(span)), evidenceLabel(res))
		return nil
	}

	batches, err := upload.SplitForUpload(redacted, upload.MaxCompressedBytes)
	if err != nil {
		return fmt.Errorf("pack %s: %w", r.ID, err)
	}
	for i, batch := range batches {
		res, err := uploadEvidence(ctx, instance, token, r.ID, span, batch.Body, len(batch.Artifacts), prompt)
		if err != nil {
			return err
		}
		part := ""
		if len(batches) > 1 {
			part = fmt.Sprintf(" (part %d/%d)", i+1, len(batches))
		}
		fmt.Fprintf(w, "  %s %s — %s sessions%s → %s\n",
			success("synced"), dim(formatSpan(span)), bold(fmt.Sprintf("%d", len(batch.Artifacts))), part, evidenceLabel(res))
	}
	return nil
}

// planWithRetry calls aiscanSyncPlan, re-authorizing once if the server rejects
// the token (mirrors run's 401 → clear → re-auth). On a refresh it updates
// *token so later calls reuse the fresh one.
func planWithRetry(ctx context.Context, instance string, token *string, source string, prompt auth.Prompt, available []syncplan.Span) ([]syncplan.Span, error) {
	needed, err := syncplan.Plan(ctx, instance, *token, source, upload.SchemaVersionV1, available)
	if errors.Is(err, syncplan.ErrUnauthorized) {
		fresh, aerr := reauthorize(ctx, instance, prompt)
		if aerr != nil {
			return nil, aerr
		}
		*token = fresh
		needed, err = syncplan.Plan(ctx, instance, fresh, source, upload.SchemaVersionV1, available)
	}
	return needed, err
}

// uploadEvidence posts one evidence body for span, re-authorizing once on a 401
// the same way run's uploadBatch does.
func uploadEvidence(ctx context.Context, instance string, token *string, id capture.SourceID, span syncplan.Span, body []byte, sessions int, prompt auth.Prompt) (*upload.EvidenceResult, error) {
	p := upload.EvidenceParams{
		InstanceURL:   instance,
		Token:         *token,
		Source:        id,
		CapturedStart: span.Start,
		CapturedEnd:   span.End,
		SchemaVersion: upload.SchemaVersionV1,
		Sessions:      sessions,
	}
	res, err := upload.UploadEvidence(ctx, p, body)
	if errors.Is(err, upload.ErrUnauthorized) {
		fresh, aerr := reauthorize(ctx, instance, prompt)
		if aerr != nil {
			return nil, aerr
		}
		*token = fresh
		p.Token = fresh
		res, err = upload.UploadEvidence(ctx, p, body)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// reauthorize clears the cached token and runs the device-code flow again,
// returning the fresh token.
func reauthorize(ctx context.Context, instance string, prompt auth.Prompt) (string, error) {
	_ = auth.ClearToken(instance)
	fresh, err := auth.EnsureToken(ctx, instance, prompt)
	if err != nil {
		return "", fmt.Errorf("re-authorize: %w", err)
	}
	return fresh, nil
}

// formatSpan renders a span as "start … end" in UTC RFC3339, for log lines.
func formatSpan(s syncplan.Span) string {
	return s.Start.UTC().Format(time.RFC3339) + " … " + s.End.UTC().Format(time.RFC3339)
}

// evidenceLabel is the evidence gid for a log line, or a placeholder when the
// server returned none.
func evidenceLabel(res *upload.EvidenceResult) string {
	if res == nil || res.EvidenceGID == "" {
		return dim("(no evidence id)")
	}
	return accent(res.EvidenceGID)
}
