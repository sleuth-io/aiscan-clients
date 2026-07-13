// Package antigravity is the capture source for Google's Antigravity, both the
// CLI and the IDE. Every surface writes the same per-conversation JSONL
// transcript format; they differ only in which ~/.gemini subdirectory holds it:
//
//	~/.gemini/antigravity-cli/   CLI
//	~/.gemini/antigravity/       IDE, legacy layout
//	~/.gemini/antigravity-ide/   IDE, 2.0+ layout
//
// Each conversation is a directory tree under the root:
//
//	<root>/history.jsonl                                    index of conversations
//	<root>/brain/<conversation-id>/.system_generated/logs/
//	  transcript.jsonl                                      truncated transcript
//	  transcript_full.jsonl                                 full transcript
//
// We collect every *.jsonl under the roots and hand it up unchanged — no parsing,
// no normalization (that is the server's job). The truncated and full transcripts
// overlap by design; the server picks the one it wants.
//
// The CLI and IDE are separate capture sources so the usage report can tell them
// apart, but they share all the walking logic below.
package antigravity

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// variant is one Antigravity surface: a source id plus the ~/.gemini
// subdirectories it stores conversations in (the IDE has two because of the
// legacy → 2.0 rename).
type variant struct {
	id   capture.SourceID
	dirs []string
}

var (
	cli = variant{id: capture.SourceAntigravityCLI, dirs: []string{"antigravity-cli"}}
	ide = variant{id: capture.SourceAntigravityIDE, dirs: []string{"antigravity", "antigravity-ide"}}
)

// CLIRecipe captures the Antigravity CLI; IDERecipe captures the Antigravity IDE.
// Register them in the recipe list to enable capture.
var (
	CLIRecipe = cli.recipe()
	IDERecipe = ide.recipe()
)

func (v variant) recipe() capture.Recipe {
	return capture.Recipe{
		ID:       v.id,
		Detect:   v.detect,
		Capture:  v.capture,
		Discover: v.discover,
	}
}

// roots returns the variant's data directories under ~/.gemini. They are scoped
// to the specific antigravity* subdirectories, never all of ~/.gemini, so we
// never touch Gemini CLI's own files (e.g. oauth_creds.json).
func (v variant) roots() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	roots := make([]string, len(v.dirs))
	for i, d := range v.dirs {
		roots[i] = filepath.Join(home, ".gemini", d)
	}
	return roots, nil
}

// detect reports whether any of the variant's data directories exist.
func (v variant) detect() bool {
	roots, err := v.roots()
	if err != nil {
		return false
	}
	for _, r := range roots {
		if info, err := os.Stat(r); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// capture walks the variant's trees and collects every *.jsonl transcript,
// dropping files outside the [Since, Until] window when one is set.
func (v variant) capture(ctx context.Context, opts capture.Options) ([]capture.Artifact, error) {
	roots, err := v.roots()
	if err != nil {
		return nil, err
	}

	var arts []capture.Artifact
	for _, r := range roots {
		walkErr := filepath.WalkDir(r, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries (incl. a missing root), keep walking
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
					return nil // explicitly ignored conversation/path
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil // skip this file, keep the rest
			}
			arts = append(arts, capture.Artifact{
				Source: v.id,
				// Mirror the on-disk layout under the source-id prefix so the dump is
				// self-describing and wireName can strip back to the native layout:
				// <source>/brain/<id>/.system_generated/logs/....
				Path: string(v.id) + "/" + filepath.ToSlash(rel),
				Data: data,
			})
			return nil
		})
		if walkErr != nil {
			return arts, walkErr
		}
	}
	return arts, nil
}

// discover walks the variant's trees and returns the earliest transcript mtime,
// the lower bound of its available span. It reads no file contents — only
// directory metadata — so it is cheap relative to a full capture. It returns the
// zero time when no transcript exists.
func (v variant) discover(ctx context.Context) (time.Time, error) {
	roots, err := v.roots()
	if err != nil {
		return time.Time{}, err
	}
	var earliest time.Time
	for _, r := range roots {
		walkErr := filepath.WalkDir(r, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries (incl. a missing root), keep walking
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
			mt := info.ModTime()
			if earliest.IsZero() || mt.Before(earliest) {
				earliest = mt
			}
			return nil
		})
		if walkErr != nil {
			return earliest, walkErr
		}
	}
	return earliest, nil
}
