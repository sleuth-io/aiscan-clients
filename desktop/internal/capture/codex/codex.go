// Package codex is the capture source for OpenAI Codex CLI session logs. It reads
// the raw JSONL rollout transcripts under ~/.codex/sessions and hands them up as
// Artifacts unchanged — no parsing, no normalization (that is the server's job,
// which has a dedicated codex parser).
package codex

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// Recipe is the Codex CLI capture source. Register it in the recipe list to
// enable Codex capture.
var Recipe = capture.Recipe{
	ID:      capture.SourceCodex,
	Detect:  detect,
	Capture: captureSessions,
}

// root returns the Codex sessions directory (~/.codex/sessions).
func root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// detect reports whether a Codex sessions directory exists.
func detect() bool {
	r, err := root()
	if err != nil {
		return false
	}
	info, err := os.Stat(r)
	return err == nil && info.IsDir()
}

// captureSessions walks the sessions tree and collects every *.jsonl rollout
// transcript, dropping files modified before opts.Since when a window is set.
// Codex nests rollouts under YYYY/MM/DD date directories, so the walk recurses.
func captureSessions(ctx context.Context, opts capture.Options) ([]capture.Artifact, error) {
	r, err := root()
	if err != nil {
		return nil, err
	}

	var arts []capture.Artifact
	walkErr := filepath.WalkDir(r, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep walking
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !opts.Since.IsZero() && info.ModTime().Before(opts.Since) {
			return nil // outside the window
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // skip this file, keep the rest
		}
		rel, err := filepath.Rel(r, path)
		if err != nil {
			rel = d.Name()
		}
		arts = append(arts, capture.Artifact{
			Source: capture.SourceCodex,
			// Mirror the on-disk layout under a per-source prefix so the dump is
			// self-describing: codex/sessions/<YYYY>/<MM>/<DD>/rollout-*.jsonl.
			Path: "codex/sessions/" + filepath.ToSlash(rel),
			Data: data,
		})
		return nil
	})
	if walkErr != nil {
		return arts, walkErr
	}
	return arts, nil
}
