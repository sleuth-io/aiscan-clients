// Package cowork is the capture source for Claude Cowork local sessions. Cowork
// (the Claude desktop app's agentic mode) stores each working session under
// <config>/Claude/local-agent-mode-sessions as a local_<uuid>.json metadata file
// plus a local_<uuid>/ directory holding audit.jsonl — the session transcript.
// We collect only those two and hand them up unchanged; parsing is the server's
// job.
//
// We deliberately leave the rest of a session on disk. .audit-key is the per-
// session HMAC signing secret. uploads/ and outputs/ are the user's own documents
// (Cowork is built for file-heavy knowledge work), so capturing them wholesale
// would put arbitrary business content on the wire. Skills and MCP usage need no
// extra files: the metadata records the configured servers/tools and audit.jsonl's
// system/init event lists the active skills, plugins, and MCP servers, so they
// ride along inside the two files we already collect.
package cowork

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// Recipe is the Claude Cowork capture source. Register it in the recipe list to
// enable Cowork capture.
var Recipe = capture.Recipe{
	ID:       capture.SourceClaudeCowork,
	Detect:   detect,
	Capture:  captureSessions,
	Discover: discover,
}

// sessionsDir is the Cowork sessions directory under one app-data base, e.g.
// ~/Library/Application Support/Claude/local-agent-mode-sessions on macOS and
// %AppData%\Claude\local-agent-mode-sessions on Windows.
const sessionsDir = "local-agent-mode-sessions"

// appDirs are the Cowork app-data directory names to probe under the OS config
// dir. "Claude" is the desktop app; "Claude-3p" is the standalone build.
var appDirs = []string{"Claude", "Claude-3p"}

// prunedDirs are directories we never descend into: the user's own files
// (uploads/outputs) and installed skill/plugin payloads. Hidden directories
// (.claude working state, .git, …) are pruned separately by their leading dot;
// these are the non-hidden ones worth naming.
var prunedDirs = map[string]bool{
	"uploads":        true,
	"outputs":        true,
	"cowork_plugins": true,
	"skills-plugin":  true,
	"debug":          true,
	"rpm":            true,
}

// roots returns the Cowork sessions directories that exist (one per app-data base
// that is present). os.UserConfigDir resolves the per-OS app-data base for free:
// ~/Library/Application Support on macOS, %AppData% on Windows — so the same code
// finds Cowork on both without a GOOS switch.
func roots() []string {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, app := range appDirs {
		dir := filepath.Join(base, app, sessionsDir)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			out = append(out, dir)
		}
	}
	return out
}

// detect reports whether any Cowork sessions directory exists.
func detect() bool {
	return len(roots()) > 0
}

// wanted reports whether a file is one we capture: a session's metadata
// (local_<uuid>.json) or its transcript (audit.jsonl). Everything else on disk —
// the .audit-key signing secret above all — is left untouched.
func wanted(name string) bool {
	if name == "audit.jsonl" {
		return true
	}
	return strings.HasPrefix(name, "local_") && strings.HasSuffix(name, ".json")
}

// captureSessions walks every Cowork sessions tree and collects each session's
// metadata and audit log, dropping files modified before opts.Since when a window
// is set. Directories holding user files or skill/plugin payloads are pruned, so
// their contents are never read even if one happens to share a captured filename.
func captureSessions(ctx context.Context, opts capture.Options) ([]capture.Artifact, error) {
	var arts []capture.Artifact
	for _, root := range roots() {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				// Prune user-file and skill/plugin subtrees (but never the root itself).
				if path != root && (strings.HasPrefix(d.Name(), ".") || prunedDirs[d.Name()]) {
					return fs.SkipDir
				}
				return nil
			}
			if !wanted(d.Name()) {
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
			data, err := os.ReadFile(path)
			if err != nil {
				return nil // skip this file, keep the rest
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = d.Name()
			}
			arts = append(arts, capture.Artifact{
				Source: capture.SourceClaudeCowork,
				// Mirror the on-disk layout under a per-source prefix so the dump is
				// self-describing and the server can pair each audit.jsonl with its
				// metadata sibling: claude-cowork/local-agent-mode-sessions/<a>/<b>/...
				Path: "claude-cowork/" + sessionsDir + "/" + filepath.ToSlash(rel),
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

// discover walks every Cowork sessions tree and returns the earliest captured
// file's mtime, the lower bound of Cowork's available span. It reads no file
// contents — only directory metadata — and prunes the same user-file and
// skill/plugin subtrees as captureSessions. It returns the zero time when no
// session file exists.
func discover(ctx context.Context) (time.Time, error) {
	var earliest time.Time
	for _, root := range roots() {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if path != root && (strings.HasPrefix(d.Name(), ".") || prunedDirs[d.Name()]) {
					return fs.SkipDir
				}
				return nil
			}
			if !wanted(d.Name()) {
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
