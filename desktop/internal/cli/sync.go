package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
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
// The server hands back the exact work-list; the client does no interval math of
// its own beyond optionally clamping the available span with --window-days /
// --until-days.
//
// With --no-upload it becomes a capture-only inspection: it collects, redacts,
// and summarizes locally without contacting the server — the same view `aiscan
// capture` gives. --no-redact (debug) forces that mode, since unredacted data is
// never sent over the wire.
func Sync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	instance := fs.String("instance", defaultInstance(), "aiscan instance URL to sync with")
	source := fs.String("source", "", "only sync these comma-separated sources (e.g. claude-cowork); empty = all")
	windowDays := fs.Int("window-days", 0, "only sync data from within the last N days (0 = no limit)")
	fs.IntVar(windowDays, "w", 0, "alias for --window-days")
	untilDays := fs.Int("until-days", 0, "only sync data modified more than N days ago (0 = up to now)")
	fs.IntVar(untilDays, "u", 0, "alias for --until-days")
	ignore := fs.String("ignore", "", "comma-separated path substrings to skip (e.g. a noisy project)")
	force := fs.Bool("force", false, "full sync: re-upload all available data, ignoring what the server already has")
	noUpload := fs.Bool("no-upload", false, "capture only: collect, redact, and summarize without uploading")
	noRedact := fs.Bool("no-redact", false, "skip secret redaction (debug; forces --no-upload)")
	showRedactions := fs.Bool("show-redactions", false, "debug: print every redacted match")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Sync local AI-tool usage to the aiscan server over the spans it still")
		fmt.Fprintln(os.Stderr, "needs. Discovers what exists, asks the server what's missing, uploads only")
		fmt.Fprintln(os.Stderr, "that. Authorizes once via the device-code flow. With --no-upload it only")
		fmt.Fprintln(os.Stderr, "collects and summarizes locally (like `aiscan capture`).")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan sync [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--instance URL", 21)), "aiscan instance to sync with (default "+defaultInstance()+")")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--source LIST", 21)), "only sync these comma-separated sources (e.g. "+knownSourceList(recipes)+"); empty = all")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("-w, --window-days N", 21)), "only sync data from within the last N days (0 = no limit)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("-u, --until-days N", 21)), "only sync data modified more than N days ago (0 = up to now)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--ignore LIST", 21)), "comma-separated path substrings to skip (e.g. a noisy project)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--force", 21)), "full sync: re-upload all available data, ignoring what the server already has")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--no-upload", 21)), "capture only: collect, redact, and summarize without uploading")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--no-redact", 21)), "skip secret redaction (debug; forces --no-upload)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--show-redactions", 21)), "debug: print every redacted match (shows the matched secret values)")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, dim("  --window-days and --until-days both count days-ago and combine into a"))
		fmt.Fprintln(os.Stderr, dim("  [window-days, until-days] slice, so until-days must be less than"))
		fmt.Fprintln(os.Stderr, dim("  window-days (e.g. --window-days 10 --until-days 5 = 10-5 days ago)."))
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// Both count days-ago, so until-days must be the smaller (more recent) bound;
	// otherwise the window is empty — reject it loudly rather than silently syncing
	// nothing.
	if *windowDays > 0 && *untilDays > 0 && *untilDays >= *windowDays {
		return fmt.Errorf("--until-days (%d) must be less than --window-days (%d): the window is [window-days, until-days] counting days ago", *untilDays, *windowDays)
	}

	selected, err := selectRecipes(recipes, *source)
	if err != nil {
		return err
	}

	var ignoreList []string
	for _, s := range strings.Split(*ignore, ",") {
		if s = strings.TrimSpace(s); s != "" {
			ignoreList = append(ignoreList, s)
		}
	}

	// Capture-only mode: --no-upload, or --no-redact (unredacted data is never
	// uploaded). Collect over the requested window and summarize locally, exactly
	// as `aiscan capture` does — no server call, no auth.
	if *noUpload || *noRedact {
		if *noRedact && !*noUpload {
			fmt.Fprintln(os.Stdout, warn("--no-redact skips upload (unredacted data is never sent)"))
		}
		opts := capture.Options{Ignore: ignoreList}
		if *windowDays > 0 {
			opts.Since = time.Now().AddDate(0, 0, -*windowDays)
		}
		if *untilDays > 0 {
			opts.Until = time.Now().AddDate(0, 0, -*untilDays)
		}
		arts, errs := capture.Run(context.Background(), selected, opts)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "%s %v\n", warn("warning:"), e)
		}
		return captureRun{noRedact: *noRedact, showRedactions: *showRedactions}.process(os.Stdout, arts)
	}

	cfg := syncConfig{instance: *instance, ignore: ignoreList, showRedactions: *showRedactions, force: *force}
	if *windowDays > 0 {
		cfg.windowSince = time.Now().UTC().AddDate(0, 0, -*windowDays)
	}
	if *untilDays > 0 {
		cfg.windowUntil = time.Now().UTC().AddDate(0, 0, -*untilDays)
	}

	ctx := context.Background()
	prompt := devicePrompt(os.Stdout)
	token, err := auth.EnsureToken(ctx, *instance, prompt)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	for _, r := range selected {
		if err := syncSource(ctx, cfg, &token, r, prompt, os.Stdout); err != nil {
			return err
		}
	}
	return nil
}

// syncConfig carries the sync-wide knobs down through the per-source and per-span
// steps: which instance, which paths to skip, whether to dump redaction matches,
// and the optional window bounds that clamp the available span reported to the
// server (zero = unbounded).
type syncConfig struct {
	instance       string
	ignore         []string
	showRedactions bool
	windowSince    time.Time // clamp available-span start (zero = earliest data)
	windowUntil    time.Time // clamp available-span end (zero = now)
	// force skips the server sync-plan and re-uploads the whole available span,
	// backfilling anything a past partial or failed sync left the server
	// missing. Safe because ingest is idempotent and dedups by session id.
	force bool
}

// syncSource runs the discover → plan → upload loop for one source. A source not
// present on this machine (or without discovery) is skipped, not an error.
func syncSource(ctx context.Context, cfg syncConfig, token *string, r capture.Recipe, prompt auth.Prompt, w io.Writer) error {
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
	// The window flags narrow what we offer the server: raise the start to the
	// window floor and lower the end to the until bound, so the server only asks
	// within the slice the user allowed.
	if !cfg.windowSince.IsZero() && cfg.windowSince.After(available.Start) {
		available.Start = cfg.windowSince
	}
	if !cfg.windowUntil.IsZero() && cfg.windowUntil.Before(available.End) {
		available.End = cfg.windowUntil
	}
	if !available.Start.Before(available.End) {
		fmt.Fprintf(w, "%s %s — window excludes all data, skipping\n", dim("sync:"), header(string(r.ID)))
		return nil
	}
	fmt.Fprintf(w, "%s %s — available %s\n", header("sync:"), header(string(r.ID)), accent(formatSpan(available)))

	var needed []syncplan.Span
	if cfg.force {
		// Full sync: ignore the server's coverage and re-upload the entire
		// available span. Ingest is idempotent and dedups by session id at
		// report time, so this backfills whatever a past partial or failed sync
		// left missing without duplicating what the server already has.
		needed = []syncplan.Span{available}
		fmt.Fprintf(w, "  %s re-uploading all available data (ignoring server coverage)\n", warn("full sync:"))
	} else {
		var perr error
		needed, perr = planWithRetry(ctx, cfg.instance, token, string(r.ID), prompt, []syncplan.Span{available})
		if perr != nil {
			return fmt.Errorf("plan %s: %w", r.ID, perr)
		}
		if len(needed) == 0 {
			fmt.Fprintf(w, "  %s up to date (no spans needed)\n", dim("·"))
			return nil
		}
	}

	for _, span := range needed {
		if err := syncSpan(ctx, cfg, token, r, span, prompt, w); err != nil {
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
func syncSpan(ctx context.Context, cfg syncConfig, token *string, r capture.Recipe, span syncplan.Span, prompt auth.Prompt, w io.Writer) error {
	arts, errs := capture.Run(ctx, []capture.Recipe{r}, capture.Options{Since: span.Start, Until: span.End, Ignore: cfg.ignore})
	for _, e := range errs {
		fmt.Fprintf(w, "%s %v\n", warn("warning:"), e)
	}

	// Redaction is the only gate before the wire; it always runs here.
	redacted, summary := redact.Redact(arts)
	if cfg.showRedactions {
		for _, m := range summary.Matches {
			fmt.Fprintf(w, "  %s %s %s\n", dim(rpad(m.Rule, 22)), m.Text, dim("← "+m.Path))
		}
	}
	if len(redacted) == 0 {
		res, err := uploadEvidence(ctx, cfg.instance, token, r.ID, span, nil, 0, prompt)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %s %s — empty → %s\n", success("synced"), dim(formatSpan(span)), evidenceLabel(res))
		return nil
	}

	uploaded, err := uploadArtifacts(ctx, cfg, token, r, span, redacted, upload.MaxCompressedBytes, prompt, w)
	if err != nil {
		return err
	}
	// Nothing landed — every session in this window was skipped as too large.
	// Deliberately do NOT post an empty body: an empty window still *covers* its
	// range (docs/upload-schema-v1.md), so marking it here would record the span
	// as done and bury the skipped sessions for good. Leave it uncovered so a
	// later sync retries it once the server cap rises or the file shrinks; the
	// skip warnings above already surfaced what was dropped and why.
	if uploaded == 0 {
		fmt.Fprintf(w, "  %s %s — nothing uploaded (all sessions too large); left for a later retry\n",
			warn("incomplete:"), dim(formatSpan(span)))
	}
	return nil
}

// uploadArtifacts uploads arts as evidence for span, packing them into batches
// that each fit maxCompressed. It skips — with a warning — any single session
// whose own compressed body exceeds the cap, rather than POST a body the server
// is bound to reject. If the server still answers 413 (a proxy or a server cap
// below our estimate), it re-splits the offending batch under half the rejected
// size and retries; a lone session the server rejects is skipped too, so one
// oversized file can't fail the whole sync. It returns the count of sessions
// actually uploaded.
func uploadArtifacts(ctx context.Context, cfg syncConfig, token *string, r capture.Recipe, span syncplan.Span, arts []capture.Artifact, maxCompressed int, prompt auth.Prompt, w io.Writer) (int, error) {
	batches, oversized, err := upload.SplitForUpload(arts, maxCompressed)
	if err != nil {
		return 0, fmt.Errorf("pack %s: %w", r.ID, err)
	}
	for _, a := range oversized {
		fmt.Fprintf(w, "  %s %s — %s, over the %s upload cap; skipped\n",
			warn("skipped"), dim(a.Path), humanBytes(len(a.Data)), humanBytes(maxCompressed))
	}

	uploaded := 0
	for _, batch := range batches {
		res, err := uploadEvidence(ctx, cfg.instance, token, r.ID, span, batch.Body, len(batch.Artifacts), prompt)
		if errors.Is(err, upload.ErrPayloadTooLarge) {
			if len(batch.Artifacts) == 1 {
				fmt.Fprintf(w, "  %s %s — %s, rejected as too large by the server; skipped\n",
					warn("skipped"), dim(batch.Artifacts[0].Path), humanBytes(len(batch.Artifacts[0].Data)))
				continue
			}
			// The server's real limit is below our cap; re-split this batch
			// under half of what it just rejected (guaranteeing progress) and
			// retry the pieces.
			n, rerr := uploadArtifacts(ctx, cfg, token, r, span, batch.Artifacts, len(batch.Body)/2, prompt, w)
			if rerr != nil {
				return uploaded, rerr
			}
			uploaded += n
			continue
		}
		if err != nil {
			return uploaded, err
		}
		uploaded += len(batch.Artifacts)
		fmt.Fprintf(w, "  %s %s — %s sessions → %s\n",
			success("synced"), dim(formatSpan(span)), bold(fmt.Sprintf("%d", len(batch.Artifacts))), evidenceLabel(res))
	}
	return uploaded, nil
}

// humanBytes renders a byte count as a compact human-readable size (e.g. 45 MiB)
// for the size notes in skip warnings.
func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := int64(n) / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// planWithRetry calls aiscanSyncPlan, re-authorizing once if the server rejects
// the token (mirrors the upload path's 401 → clear → re-auth). On a refresh it
// updates *token so later calls reuse the fresh one.
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

// uploadEvidence posts one evidence body for span, re-authorizing once on a 401.
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

// errAuthRequired means the server rejected the token and the caller is
// non-interactive (daemon), so re-running the device flow — which needs a
// human in a browser — is not an option. The stale token is already cleared.
var errAuthRequired = errors.New("authorization required")

// reauthorize clears the cached token and runs the device-code flow again,
// returning the fresh token. A nil prompt marks a non-interactive caller:
// it fails with errAuthRequired instead of silently starting a device flow
// nobody will approve.
func reauthorize(ctx context.Context, instance string, prompt auth.Prompt) (string, error) {
	_ = auth.ClearToken(instance)
	if prompt == nil {
		return "", errAuthRequired
	}
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
