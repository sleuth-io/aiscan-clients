package cursor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"

	_ "modernc.org/sqlite"
)

// withStore points os.UserConfigDir at a temp dir and writes a Cursor-shaped
// state.vscdb there holding one composer with two messages (inserted out of
// conversation order to exercise ordering). lastUpdatedAt is settable so the
// window filter can be tested.
// storeDB points os.UserConfigDir at a temp dir and returns an open, empty Cursor
// store. The caller inserts rows and must Close it before invoking capture (which
// opens its own read-only snapshot of the file).
func storeDB(t *testing.T) *sql.DB {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)          // unix: UserConfigDir -> $HOME/.config
	t.Setenv("USERPROFILE", home)   // windows
	t.Setenv("XDG_CONFIG_HOME", "") // force the $HOME/.config fallback on linux

	path, err := dbPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE cursorDiskKV (key TEXT UNIQUE, value BLOB)"); err != nil {
		t.Fatal(err)
	}
	return db
}

func withStore(t *testing.T, lastUpdatedAt int64) {
	t.Helper()
	db := storeDB(t)
	defer db.Close()
	composer := map[string]any{
		"composerId":    "comp-1",
		"createdAt":     lastUpdatedAt - 1000,
		"lastUpdatedAt": lastUpdatedAt,
		// Conversation order is b2 then b1 — the reverse of the insert order below.
		"fullConversationHeadersOnly": []map[string]any{
			{"bubbleId": "b2"},
			{"bubbleId": "b1"},
		},
	}
	insert(t, db, "composerData:comp-1", composer)
	insert(t, db, "bubbleId:comp-1:b1", map[string]any{"bubbleId": "b1", "type": 1, "text": "hello"})
	insert(t, db, "bubbleId:comp-1:b2", map[string]any{"bubbleId": "b2", "type": 2, "text": "hi"})
	// A second composer's bubble must not leak into comp-1's session.
	insert(t, db, "composerData:comp-2", map[string]any{"composerId": "comp-2", "lastUpdatedAt": lastUpdatedAt})
	insert(t, db, "bubbleId:comp-2:z1", map[string]any{"bubbleId": "z1", "type": 1})
}

func insert(t *testing.T, db *sql.DB, key string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	insertRaw(t, db, key, data)
}

func insertRaw(t *testing.T, db *sql.DB, key string, value []byte) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)", key, value); err != nil {
		t.Fatal(err)
	}
}

func captureAll(t *testing.T, opts capture.Options) []capture.Artifact {
	t.Helper()
	arts, err := captureSessions(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return arts
}

func TestCaptureSkipsUnreadableComposerRow(t *testing.T) {
	db := storeDB(t)
	insertRaw(t, db, "composerData:bad", []byte("this is not json"))
	insert(t, db, "composerData:good", map[string]any{"composerId": "good", "lastUpdatedAt": 1})
	db.Close()

	arts := captureAll(t, capture.Options{})
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (the unreadable row is skipped)", len(arts))
	}
	if arts[0].Path != "cursor/composers/good.jsonl" {
		t.Errorf("path = %q, want the readable composer", arts[0].Path)
	}
}

func TestCaptureFallsBackToKeyWhenComposerIdMissing(t *testing.T) {
	db := storeDB(t)
	// No composerId field -> the id comes from the cursorDiskKV key suffix.
	insert(t, db, "composerData:from-key", map[string]any{"lastUpdatedAt": 1, "text": "x"})
	insert(t, db, "bubbleId:from-key:b1", map[string]any{"bubbleId": "b1", "type": 1, "text": "hi"})
	db.Close()

	arts := captureAll(t, capture.Options{})
	if len(arts) != 1 || arts[0].Path != "cursor/composers/from-key.jsonl" {
		t.Fatalf("got %v, want path derived from the key suffix", pathsOf(arts))
	}
}

func TestCaptureIgnoreFiltersComposer(t *testing.T) {
	db := storeDB(t)
	insert(t, db, "composerData:keep-me", map[string]any{"composerId": "keep-me", "lastUpdatedAt": 1, "text": "x"})
	insert(t, db, "composerData:noisy-one", map[string]any{"composerId": "noisy-one", "lastUpdatedAt": 1, "text": "x"})
	db.Close()

	arts := captureAll(t, capture.Options{Ignore: []string{"noisy"}})
	if len(arts) != 1 || arts[0].Path != "cursor/composers/keep-me.jsonl" {
		t.Fatalf("got %v, want only the non-ignored composer", pathsOf(arts))
	}
}

func TestCaptureUntilFiltersTooNew(t *testing.T) {
	now := time.Now().UnixMilli()
	db := storeDB(t)
	insert(t, db, "composerData:fresh", map[string]any{"composerId": "fresh", "lastUpdatedAt": now, "text": "x"})
	db.Close()

	// Until is the recent bound; a composer updated "now" is newer than it -> dropped.
	arts := captureAll(t, capture.Options{Until: time.Now().AddDate(0, 0, -1)})
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (composer is newer than --until)", len(arts))
	}
}

func TestCaptureDropsMalformedBubbleKeepsSiblings(t *testing.T) {
	db := storeDB(t)
	insert(t, db, "composerData:c1", map[string]any{
		"composerId":    "c1",
		"lastUpdatedAt": 1,
		"fullConversationHeadersOnly": []map[string]any{
			{"bubbleId": "b1"},
			{"bubbleId": "b2"},
		},
	})
	insertRaw(t, db, "bubbleId:c1:b1", []byte("{ broken json"))
	insert(t, db, "bubbleId:c1:b2", map[string]any{"bubbleId": "b2", "type": 1, "text": "ok"})
	db.Close()

	arts := captureAll(t, capture.Options{})
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
	lines := nonEmptyLines(arts[0].Data)
	// composer line + only the well-formed bubble; the broken one is dropped.
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (composer + 1 valid bubble)", len(lines))
	}
	if id := bubbleID(t, lines[1]); id != "b2" {
		t.Errorf("kept bubble = %q, want b2", id)
	}
}

func pathsOf(arts []capture.Artifact) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.Path
	}
	return out
}

func TestCaptureGroupsBubblesByComposerInOrder(t *testing.T) {
	now := time.Now().UnixMilli()
	withStore(t, now)

	if !detect() {
		t.Fatal("detect() = false, want true")
	}
	arts, err := captureSessions(context.Background(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want 2 (one per composer)", len(arts))
	}

	a := artifactFor(t, arts, "cursor/composers/comp-1.jsonl")
	if a.Source != capture.SourceCursor {
		t.Errorf("Source = %q, want %q", a.Source, capture.SourceCursor)
	}
	lines := nonEmptyLines(a.Data)
	// Line 1 is the composer row; the rest are its messages (no comp-2 leakage).
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (composer + 2 bubbles)", len(lines))
	}
	var composer struct {
		ComposerID string `json:"composerId"`
	}
	if err := json.Unmarshal(lines[0], &composer); err != nil {
		t.Fatalf("composer line is not valid JSON: %v", err)
	}
	if composer.ComposerID != "comp-1" {
		t.Errorf("first line composerId = %q, want comp-1", composer.ComposerID)
	}
	// Bubbles must follow fullConversationHeadersOnly order (b2 before b1).
	if id := bubbleID(t, lines[1]); id != "b2" {
		t.Errorf("first bubble = %q, want b2 (conversation order, not insert order)", id)
	}
	if id := bubbleID(t, lines[2]); id != "b1" {
		t.Errorf("second bubble = %q, want b1", id)
	}
}

func nonEmptyLines(data []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func TestCaptureWindowFiltersOld(t *testing.T) {
	old := time.Now().AddDate(0, 0, -10).UnixMilli()
	withStore(t, old)

	arts, err := captureSessions(context.Background(), capture.Options{Since: time.Now().AddDate(0, 0, -1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (composers older than the window)", len(arts))
	}
}

func artifactFor(t *testing.T, arts []capture.Artifact, path string) capture.Artifact {
	t.Helper()
	for _, a := range arts {
		if a.Path == path {
			return a
		}
	}
	t.Fatalf("no artifact with path %q", path)
	return capture.Artifact{}
}

func bubbleID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b struct {
		BubbleID string `json:"bubbleId"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b.BubbleID
}
