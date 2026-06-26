// Package cli holds the command-line verbs for the desktop client.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture/claude"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/redact"
)

// recipes is the enabled capture sources. Adding a source = one line here.
var recipes = []capture.Recipe{
	claude.Recipe,
}

// Capture implements `aiscan capture`: collect raw artifacts from every
// available source and, with --out, write them to a directory for inspection.
// It stops before redact/upload — those are separate steps.
func Capture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	out := fs.String("out", "", "directory to write collected artifacts to (omit to only summarize)")
	windowDays := fs.Int("window-days", 0, "only collect files modified within the last N days (0 = no limit)")
	noRedact := fs.Bool("no-redact", false, "skip secret redaction (debug; shows raw captured bytes)")
	showRedactions := fs.Bool("show-redactions", false, "debug: print every redacted match")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Collect local AI-tool usage and redact obvious secrets. Read-only;")
		fmt.Fprintln(os.Stderr, "does not upload (a separate step). With --out, writes the redacted dump.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan capture [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--out DIR", 19)), "write collected artifacts to DIR (omit to only summarize)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--window-days N", 19)), "only collect files modified within the last N days (0 = no limit)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--no-redact", 19)), "skip secret redaction (debug; shows raw captured bytes)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--show-redactions", 19)), "debug: print every redacted match (shows the matched secret values)")
	}
	if err := fs.Parse(args); err != nil {
		// -h / --help is not an error: flag already printed usage.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	opts := capture.Options{}
	if *windowDays > 0 {
		opts.Since = time.Now().AddDate(0, 0, -*windowDays)
	}

	arts, errs := capture.Run(context.Background(), recipes, opts)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "%s %v\n", warn("warning:"), e)
	}

	// Redact secrets before anything is shown or written — this is the only
	// gate before the wire, so it runs by default. --no-redact is debug-only.
	var redacted redact.Summary
	if !*noRedact {
		arts, redacted = redact.Redact(arts)
	}

	// Per-source summary.
	counts := map[capture.SourceID]int{}
	var bytes int
	for _, a := range arts {
		counts[a.Source]++
		bytes += len(a.Data)
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("%-14s %s artifacts\n", header(id), bold(strconv.Itoa(counts[capture.SourceID(id)])))
	}
	fmt.Printf("%s %s artifacts, %s bytes\n", dim("total:"), bold(strconv.Itoa(len(arts))), bold(strconv.Itoa(bytes)))

	// Redaction summary — the trust surface: what was stripped before the wire.
	if *noRedact {
		fmt.Println(warn("redaction: skipped (--no-redact)"))
	} else if n := redacted.Total(); n > 0 {
		parts := make([]string, 0, len(redacted.Counts))
		for _, name := range redacted.Applied() {
			parts = append(parts, fmt.Sprintf("%s %d", name, redacted.Counts[name]))
		}
		fmt.Printf("%s %s (%s)\n", header("redacted:"), bold(strconv.Itoa(n)), dim(strings.Join(parts, ", ")))
	} else {
		fmt.Println(dim("redacted: nothing matched"))
	}

	// Debug: dump every match so false positives are visible, tagged with the
	// artifact (project/session) it was redacted from.
	if *showRedactions {
		for _, m := range redacted.Matches {
			fmt.Printf("  %s %s %s\n", dim(rpad(m.Rule, 22)), m.Text, dim("← "+m.Path))
		}
	}

	if *out != "" {
		if err := writeArtifacts(*out, arts); err != nil {
			return err
		}
		fmt.Printf("%s %d artifacts to %s\n", success("wrote"), len(arts), *out)
	}
	return nil
}

// writeArtifacts dumps artifacts under dir, preserving their logical paths.
func writeArtifacts(dir string, arts []capture.Artifact) error {
	for _, a := range arts {
		dest := filepath.Join(dir, filepath.FromSlash(a.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, a.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
