// Package cli holds the command-line verbs for the desktop client.
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := capture.Options{}
	if *windowDays > 0 {
		opts.Since = time.Now().AddDate(0, 0, -*windowDays)
	}

	arts, errs := capture.Run(context.Background(), recipes, opts)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
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
		fmt.Printf("%-14s %d artifacts\n", id, counts[capture.SourceID(id)])
	}
	fmt.Printf("total: %d artifacts, %d bytes\n", len(arts), bytes)

	if *out != "" {
		if err := writeArtifacts(*out, arts); err != nil {
			return err
		}
		fmt.Printf("wrote %d artifacts to %s\n", len(arts), *out)
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
