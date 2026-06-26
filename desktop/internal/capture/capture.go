// Package capture defines the source-agnostic capture seam: a Recipe describes
// how to detect and read one AI tool's local usage data, and Run executes a set
// of recipes into a flat list of Artifacts.
//
// Every source produces the same Artifact type, so the downstream shared steps
// (redact, upload) operate on Artifacts and never need to know which source
// produced them. Adding a new source means adding a Recipe — nothing in redact
// or upload changes.
package capture

import (
	"context"
	"fmt"
	"time"
)

// SourceID identifies a capture source on the wire. The server uses it to pick
// the right parser; the client treats it as an opaque label.
type SourceID string

const (
	SourceClaudeCode SourceID = "claude-code"
	SourceCodex      SourceID = "codex"
	// SourceCursor, SourceCopilot, ... land here as they are implemented.
)

// Artifact is one raw, un-normalized file collected from a source. Data is the
// bytes exactly as found on disk — parsing and normalization are server-side.
type Artifact struct {
	Source SourceID // which source produced this
	Path   string   // logical path within the upload dump, slash-separated
	Data   []byte   // raw bytes, not normalized
}

// Options tunes a capture run.
type Options struct {
	// Since, if non-zero, drops artifacts last modified before it.
	Since time.Time
}

// Recipe is the declarative description of one source. Detect reports whether
// the tool is present on this machine; Capture reads its artifacts. Expressing
// sources as data (a slice of Recipes) keeps "add a source" to one list entry.
type Recipe struct {
	ID      SourceID
	Detect  func() bool
	Capture func(ctx context.Context, opts Options) ([]Artifact, error)
}

// Run executes every available recipe and concatenates their artifacts. A
// recipe that fails contributes an error but does not abort the others — a
// broken source must not block the rest of the capture.
func Run(ctx context.Context, recipes []Recipe, opts Options) ([]Artifact, []error) {
	var arts []Artifact
	var errs []error
	for _, r := range recipes {
		if r.Detect != nil && !r.Detect() {
			continue // tool not installed; silently skip
		}
		got, err := r.Capture(ctx, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.ID, err))
			continue
		}
		arts = append(arts, got...)
	}
	return arts, errs
}
