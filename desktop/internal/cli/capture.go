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
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture/claude"
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
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Collect local AI-tool usage and optionally write a raw dump for")
		fmt.Fprintln(os.Stderr, "inspection. Read-only; does not redact or upload (separate steps).")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan capture [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--out DIR", 17)), "write collected artifacts to DIR (omit to only summarize)")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--window-days N", 17)), "only collect files modified within the last N days (0 = no limit)")
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
