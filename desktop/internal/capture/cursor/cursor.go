// Package cursor is the capture source for the Cursor editor's agent ("composer")
// chats. Unlike Claude Code or Codex, Cursor does not keep one file per session:
// every composer and message lives in a single SQLite store
// (<config>/Cursor/User/globalStorage/state.vscdb), in a key/value table
// (cursorDiskKV) whose values are JSON.
//
// To stay a thin, auditable client we still ship raw — we do not parse or
// normalize the chats. We only read the DB's own rows and regroup them by
// composer into one JSONL file per session, because the shared redact and window
// steps work on text artifacts (a raw 38 MB binary .vscdb could neither be
// length-preserving-redacted nor sliced by a time window). Each file holds the
// composer's row on the first line, then its message rows (in conversation
// order) one per line, all verbatim; the server parses.
//
// JSONL — not one big JSON object — because redaction is a length-changing pass
// over raw bytes that can occasionally corrupt a value's JSON (e.g. cutting an
// adjacent \uXXXX escape). Line-delimiting confines any such damage to a single
// message, exactly as the file-per-line Claude Code / Codex sources already rely
// on the server skipping unparsable lines.
package cursor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0), registered as "sqlite"
)

// Recipe is the Cursor capture source. Register it in the recipe list to enable
// Cursor capture.
var Recipe = capture.Recipe{
	ID:      capture.SourceCursor,
	Detect:  detect,
	Capture: captureSessions,
}

// dbPath returns the Cursor global storage SQLite file. os.UserConfigDir resolves
// the per-OS app-data base for free (~/.config on Linux, ~/Library/Application
// Support on macOS, %AppData% on Windows), so the same path finds Cursor on all
// three.
func dbPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Cursor", "User", "globalStorage", "state.vscdb"), nil
}

// detect reports whether the Cursor global storage DB exists.
func detect() bool {
	p, err := dbPath()
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// composerMeta is the slice of a composerData row we read to window and order a
// session. Everything else in the row is shipped verbatim, untouched.
type composerMeta struct {
	ComposerID                  string `json:"composerId"`
	CreatedAt                   int64  `json:"createdAt"`     // epoch ms
	LastUpdatedAt               int64  `json:"lastUpdatedAt"` // epoch ms
	FullConversationHeadersOnly []struct {
		BubbleID string `json:"bubbleId"`
	} `json:"fullConversationHeadersOnly"`
}

// captureSessions reads the Cursor store and emits one JSONL artifact per composer
// whose last activity falls inside the window. Cursor may be running and writing
// the live DB, so we read a consistent snapshot rather than the live file.
func captureSessions(ctx context.Context, opts capture.Options) ([]capture.Artifact, error) {
	src, err := dbPath()
	if err != nil {
		return nil, err
	}

	snap, cleanup, err := snapshot(ctx, src)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	db, err := sql.Open("sqlite", snap)
	if err != nil {
		return nil, fmt.Errorf("open cursor db: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'")
	if err != nil {
		return nil, fmt.Errorf("read composers: %w", err)
	}
	defer rows.Close()

	var arts []capture.Artifact
	for rows.Next() {
		if ctx.Err() != nil {
			return arts, ctx.Err()
		}
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return arts, err
		}
		art, ok, err := buildArtifact(ctx, db, key, value, opts)
		if err != nil {
			return arts, err
		}
		if ok {
			arts = append(arts, art)
		}
	}
	if err := rows.Err(); err != nil {
		return arts, err
	}
	return arts, nil
}

// buildArtifact turns one composerData row into a session artifact, or reports
// ok=false when the composer falls outside the window or the ignore list.
func buildArtifact(ctx context.Context, db *sql.DB, key string, value []byte, opts capture.Options) (capture.Artifact, bool, error) {
	var meta composerMeta
	if err := json.Unmarshal(value, &meta); err != nil {
		return capture.Artifact{}, false, nil // unreadable row; skip, keep the rest
	}
	// Composer ids are Cursor-assigned UUIDs (the cursorDiskKV key suffix), so they
	// are safe to interpolate into the artifact path and the bubble LIKE pattern
	// below without sanitizing for path separators or LIKE metacharacters.
	id := meta.ComposerID
	if id == "" {
		id = key[len("composerData:"):]
	}

	// Window by last activity (fall back to creation), mirroring the file-mtime
	// window the file-based sources use.
	when := meta.LastUpdatedAt
	if when == 0 {
		when = meta.CreatedAt
	}
	if !opts.Since.IsZero() && when != 0 && when < opts.Since.UnixMilli() {
		return capture.Artifact{}, false, nil
	}
	if !opts.Until.IsZero() && when != 0 && when > opts.Until.UnixMilli() {
		return capture.Artifact{}, false, nil
	}
	// We have no project path on the client, so --ignore matches the composer id.
	for _, ig := range opts.Ignore {
		if ig != "" && strings.Contains(id, ig) {
			return capture.Artifact{}, false, nil
		}
	}

	byID, err := loadBubbles(ctx, db, id)
	if err != nil {
		return capture.Artifact{}, false, err
	}

	// First line: the composer row. Following lines: its messages in conversation
	// order. compact guarantees each record is a single physical line.
	var buf bytes.Buffer
	writeLine(&buf, value)
	for _, h := range meta.FullConversationHeadersOnly {
		if raw, ok := byID[h.BubbleID]; ok {
			writeLine(&buf, raw)
		}
	}

	return capture.Artifact{
		Source: capture.SourceCursor,
		// Self-describing layout under a per-source prefix: cursor/composers/<id>.jsonl.
		Path: "cursor/composers/" + id + ".jsonl",
		Data: buf.Bytes(),
	}, true, nil
}

// writeLine appends one JSON value as a compact single line. Invalid JSON (which
// json.Compact would reject) is skipped rather than written malformed.
func writeLine(buf *bytes.Buffer, raw []byte) {
	start := buf.Len()
	if err := json.Compact(buf, raw); err != nil {
		buf.Truncate(start)
		return
	}
	buf.WriteByte('\n')
}

// loadBubbles fetches every message row for a composer, keyed by bubble id, with
// each row's exact JSON bytes preserved.
func loadBubbles(ctx context.Context, db *sql.DB, composerID string) (map[string][]byte, error) {
	rows, err := db.QueryContext(ctx, "SELECT key, value FROM cursorDiskKV WHERE key LIKE ?", "bubbleId:"+composerID+":%")
	if err != nil {
		return nil, fmt.Errorf("read bubbles: %w", err)
	}
	defer rows.Close()

	prefix := "bubbleId:" + composerID + ":"
	byID := map[string][]byte{}
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		// Scan reuses the value buffer across rows; copy before keeping it.
		byID[key[len(prefix):]] = append([]byte(nil), value...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return byID, nil
}

// snapshot writes a consistent, standalone copy of the live store to a temp dir
// and returns its path plus a cleanup. It opens the source read-only (so a running
// Cursor is never disturbed or blocked) and uses SQLite's VACUUM INTO, which takes
// a read transaction and emits one internally-consistent file — unlike copying
// state.vscdb alongside its -wal/-shm, which can race a checkpoint and produce a
// torn snapshot that silently drops the newest messages.
func snapshot(ctx context.Context, src string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "aiscan-cursor-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	out := filepath.Join(dir, "snapshot.db")
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("open cursor db: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", out); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("snapshot cursor db: %w", err)
	}
	return out, cleanup, nil
}
