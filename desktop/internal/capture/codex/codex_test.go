package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// withHome points os.UserHomeDir at a temp dir holding a fake codex sessions
// tree (nested under YYYY/MM/DD like the real Codex CLI).
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows
	day := filepath.Join(home, ".codex", "sessions", "2026", "06", "01")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-06-01T10-00-00-abc.jsonl"), []byte(`{"type":"session_meta"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	return day
}

func TestCaptureCollectsNestedJSONLOnly(t *testing.T) {
	withHome(t)
	if !detect() {
		t.Fatal("detect() = false, want true")
	}
	arts, err := captureSessions(context.Background(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (only the .jsonl)", len(arts))
	}
	a := arts[0]
	if a.Source != capture.SourceCodex {
		t.Errorf("Source = %q, want %q", a.Source, capture.SourceCodex)
	}
	want := "codex/sessions/2026/06/01/rollout-2026-06-01T10-00-00-abc.jsonl"
	if a.Path != want {
		t.Errorf("Path = %q, want %q", a.Path, want)
	}
}

func TestCaptureWindowFiltersOld(t *testing.T) {
	day := withHome(t)
	old := filepath.Join(day, "rollout-old.jsonl")
	if err := os.WriteFile(old, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	arts, err := captureSessions(context.Background(), capture.Options{Since: time.Now().AddDate(0, 0, -1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (old one filtered out)", len(arts))
	}
}

func TestDetectMissingDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.codex/sessions
	t.Setenv("USERPROFILE", t.TempDir())
	if detect() {
		t.Error("detect() = true with no ~/.codex/sessions, want false")
	}
}
