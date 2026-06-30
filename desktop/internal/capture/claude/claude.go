// Package claude is the capture source for Claude Code session logs. It reads
// the raw JSONL transcripts under ~/.claude/projects and hands them up as
// Artifacts unchanged — no parsing, no normalization (that is the server's job).
package claude

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// Recipe is the Claude Code capture source. Register it in the recipe list to
// enable Claude Code capture.
var Recipe = capture.Recipe{
	ID:      capture.SourceClaudeCode,
	Detect:  detect,
	Capture: captureSessions,
}

// root returns the Claude Code projects directory (~/.claude/projects).
func root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// detect reports whether a Claude Code projects directory exists.
func detect() bool {
	r, err := root()
	if err != nil {
		return false
	}
	info, err := os.Stat(r)
	return err == nil && info.IsDir()
}

// captureSessions walks the projects tree and collects every *.jsonl transcript,
// dropping files modified before opts.Since when a window is set.
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
			return nil // before the window
		}
		if !opts.Until.IsZero() && info.ModTime().After(opts.Until) {
			return nil // after the window
		}
		rel, err := filepath.Rel(r, path)
		if err != nil {
			rel = d.Name()
		}
		for _, ig := range opts.Ignore {
			if ig != "" && strings.Contains(rel, ig) {
				return nil // explicitly ignored project/path
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // skip this file, keep the rest
		}
		arts = append(arts, capture.Artifact{
			Source: capture.SourceClaudeCode,
			// Mirror the on-disk layout under a per-source prefix so the dump
			// is self-describing: claude-code/projects/<proj>/<session>.jsonl
			Path: "claude-code/projects/" + filepath.ToSlash(rel),
			Data: data,
		})
		return nil
	})
	if walkErr != nil {
		return arts, walkErr
	}
	return arts, nil
}
