package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/auth"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/redact"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/upload"
)

// defaultInstance is the production aiscan instance the client targets unless
// --instance overrides it (matches the extension's default).
const defaultInstance = "https://app.skills.new"

// Login implements `aiscan login`: authorize this machine against an aiscan
// instance via the device-code flow and cache the resulting token, so a later
// `aiscan run` uploads without an interactive step. (run also authorizes on
// first use, so login is optional — it just front-loads the approval.)
func Login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	instance := fs.String("instance", defaultInstance, "aiscan instance URL to authorize against")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Authorize this machine against an aiscan instance (device-code OAuth)")
		fmt.Fprintln(os.Stderr, "and cache the token for later uploads.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan login [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--instance URL", 19)), "aiscan instance to authorize against (default "+defaultInstance+")")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if _, err := auth.EnsureToken(context.Background(), *instance, devicePrompt(os.Stdout)); err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	fmt.Fprintf(os.Stdout, "%s authorized for %s\n", success("✓"), accent(strings.TrimRight(*instance, "/")))
	return nil
}

// Run implements `aiscan run`: capture local AI usage, redact secrets, and
// upload the redacted dump to the aiscan server, authorizing via the device-code
// flow on first use. Redaction always runs — there is no opt-out; it is the only
// gate before the wire.
func Run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	instance := fs.String("instance", defaultInstance, "aiscan instance URL to upload to")
	windowDays := fs.Int("window-days", 0, "only capture files modified within the last N days (0 = no limit)")
	source := fs.String("source", "", "only capture these comma-separated sources (e.g. claude-cowork); empty = all")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Capture local AI-tool usage, redact obvious secrets, and upload the")
		fmt.Fprintln(os.Stderr, "redacted dump for analysis. Authorizes once via the device-code flow.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan run [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--instance URL", 19)), "aiscan instance to upload to (default "+defaultInstance+")")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--window-days N", 19)), "only capture files modified within the last N days (0 = no limit)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--source LIST", 19)), "only capture these comma-separated sources (e.g. "+knownSourceList(recipes)+"); empty = all")
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

	opts := capture.Options{}
	if *windowDays > 0 {
		opts.Since = time.Now().AddDate(0, 0, -*windowDays)
	}

	ctx := context.Background()
	arts, errs := capture.Run(ctx, selected, opts)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "%s %v\n", warn("warning:"), e)
	}

	// Redaction is the only gate before the wire; it always runs here.
	redacted, summary := redact.Redact(arts)
	printRedactionSummary(os.Stdout, redacted, summary)
	if len(redacted) == 0 {
		fmt.Fprintln(os.Stdout, dim("nothing to upload"))
		return nil
	}

	// Each source uploads under its own parser label, so a mixed capture sends
	// each batch to the matching server parser.
	bySource := map[capture.SourceID][]capture.Artifact{}
	for _, a := range redacted {
		bySource[a.Source] = append(bySource[a.Source], a)
	}

	prompt := devicePrompt(os.Stdout)
	token, err := auth.EnsureToken(ctx, *instance, prompt)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	for _, id := range sortedSourceIDs(bySource) {
		results, err := uploadSource(ctx, *instance, &token, id, *windowDays, bySource[id], prompt)
		if err != nil {
			return err
		}
		reportResults(os.Stdout, id, results)
	}
	return nil
}

// uploadSource uploads one source's artifacts, first splitting them so each
// gzipped body fits under the server's size limit (the heavy-history case), then
// uploading each batch with adaptive fallback.
func uploadSource(ctx context.Context, instance string, token *string, id capture.SourceID, windowDays int, arts []capture.Artifact, prompt auth.Prompt) ([]*upload.Result, error) {
	batches, err := upload.SplitForUpload(arts, upload.MaxCompressedBytes)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", id, err)
	}
	var out []*upload.Result
	for _, batch := range batches {
		rs, err := uploadAdaptive(ctx, instance, token, id, windowDays, batch, prompt)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// uploadAdaptive uploads one batch, and if the server still rejects it as too
// large (413 — e.g. a proxy caps the body below our estimate), halves the batch
// and retries each half (re-packing the halves). A lone session that's still too
// big is a clear error rather than an opaque 413.
func uploadAdaptive(ctx context.Context, instance string, token *string, id capture.SourceID, windowDays int, batch upload.Batch, prompt auth.Prompt) ([]*upload.Result, error) {
	res, err := uploadBatch(ctx, instance, token, id, windowDays, batch, prompt)
	if errors.Is(err, upload.ErrPayloadTooLarge) {
		if len(batch.Artifacts) <= 1 {
			return nil, fmt.Errorf("a single %s session is too large to upload; the server rejected it (413)", id)
		}
		mid := len(batch.Artifacts) / 2
		var out []*upload.Result
		for _, half := range [][]capture.Artifact{batch.Artifacts[:mid], batch.Artifacts[mid:]} {
			subBatches, err := upload.SplitForUpload(half, upload.MaxCompressedBytes)
			if err != nil {
				return nil, err
			}
			for _, sub := range subBatches {
				rs, err := uploadAdaptive(ctx, instance, token, id, windowDays, sub, prompt)
				if err != nil {
					return nil, err
				}
				out = append(out, rs...)
			}
		}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return []*upload.Result{res}, nil
}

// reportResults prints the per-source upload outcome. A large history may upload
// in several parts (the server caps body size), so each part's report is shown.
func reportResults(w io.Writer, id capture.SourceID, results []*upload.Result) {
	if len(results) == 0 {
		return
	}
	sessions := 0
	for _, r := range results {
		sessions += r.Sessions
	}
	if len(results) == 1 {
		fmt.Fprintf(w, "%s %s — %s sessions → %s\n",
			success("uploaded"), header(string(id)), bold(strconv.Itoa(sessions)), accent(results[0].ReportURL))
		return
	}
	fmt.Fprintf(w, "%s %s — %s sessions in %s parts (server size limit):\n",
		success("uploaded"), header(string(id)), bold(strconv.Itoa(sessions)), bold(strconv.Itoa(len(results))))
	for _, r := range results {
		fmt.Fprintf(w, "  %s %s sessions → %s\n", dim("part:"), strconv.Itoa(r.Sessions), accent(r.ReportURL))
	}
}

// uploadBatch uploads one source's artifacts, re-authorizing once if the server
// rejects the token (mirrors the extension's 401 → clear → re-auth). On a
// refresh it updates *token so later batches reuse the fresh one.
func uploadBatch(ctx context.Context, instance string, token *string, id capture.SourceID, windowDays int, batch upload.Batch, prompt auth.Prompt) (*upload.Result, error) {
	p := upload.Params{InstanceURL: instance, Token: *token, Source: id, WindowDays: windowDays, Artifacts: batch.Artifacts}
	res, err := upload.UploadPacked(ctx, p, batch.Body)
	if errors.Is(err, upload.ErrUnauthorized) {
		_ = auth.ClearToken(instance)
		fresh, aerr := auth.EnsureToken(ctx, instance, prompt)
		if aerr != nil {
			return nil, fmt.Errorf("re-authorize: %w", aerr)
		}
		*token = fresh
		p.Token = fresh
		res, err = upload.UploadPacked(ctx, p, batch.Body)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// devicePrompt returns an auth.Prompt that tells the user how to approve the
// device authorization and opens the approval page in their browser.
func devicePrompt(w io.Writer) auth.Prompt {
	return func(userCode, verifyURL string) {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s open %s\n", header("authorize:"), accent(verifyURL))
		if userCode != "" {
			fmt.Fprintf(w, "           and confirm the code %s\n", bold(userCode))
		}
		if err := auth.OpenBrowser(verifyURL); err == nil {
			fmt.Fprintln(w, dim("           (opened your browser…)"))
		}
		fmt.Fprintln(w, dim("waiting for approval…"))
	}
}

// printRedactionSummary writes the trust surface (artifacts per source, bytes,
// and what redaction stripped) to w — the same shape `aiscan capture` prints.
func printRedactionSummary(w io.Writer, arts []capture.Artifact, s redact.Summary) {
	counts := map[capture.SourceID]int{}
	var nbytes int
	for _, a := range arts {
		counts[a.Source]++
		nbytes += len(a.Data)
	}
	for _, id := range sortedSourceIDs(counts) {
		fmt.Fprintf(w, "%-14s %s artifacts\n", header(string(id)), bold(strconv.Itoa(counts[id])))
	}
	fmt.Fprintf(w, "%s %s artifacts, %s bytes\n", dim("total:"), bold(strconv.Itoa(len(arts))), bold(strconv.Itoa(nbytes)))
	if n := s.Total(); n > 0 {
		parts := make([]string, 0, len(s.Counts))
		for _, name := range s.Applied() {
			parts = append(parts, fmt.Sprintf("%s %d", name, s.Counts[name]))
		}
		fmt.Fprintf(w, "%s %s (%s)\n", header("redacted:"), bold(strconv.Itoa(n)), dim(strings.Join(parts, ", ")))
	} else {
		fmt.Fprintln(w, dim("redacted: nothing matched"))
	}
}

// selectRecipes filters recipes to the comma-separated source list, or returns
// them all when the list is empty. An unrecognized source is an error (listing
// the known ones) so a typo'd --source fails loudly instead of silently
// capturing nothing.
func selectRecipes(recipes []capture.Recipe, csv string) ([]capture.Recipe, error) {
	if strings.TrimSpace(csv) == "" {
		return recipes, nil
	}
	want := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	var out []capture.Recipe
	matched := map[string]bool{}
	for _, r := range recipes {
		if want[string(r.ID)] {
			out = append(out, r)
			matched[string(r.ID)] = true
		}
	}
	var unknown []string
	for s := range want {
		if !matched[s] {
			unknown = append(unknown, s)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown --source %s; known sources: %s",
			strings.Join(unknown, ", "), knownSourceList(recipes))
	}
	return out, nil
}

// knownSourceList is the sorted, comma-separated source ids for help text and
// error messages.
func knownSourceList(recipes []capture.Recipe) string {
	ids := make([]string, 0, len(recipes))
	for _, r := range recipes {
		ids = append(ids, string(r.ID))
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// sortedSourceIDs returns m's keys sorted, so output ordering is deterministic.
func sortedSourceIDs[V any](m map[capture.SourceID]V) []capture.SourceID {
	ids := make([]capture.SourceID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
