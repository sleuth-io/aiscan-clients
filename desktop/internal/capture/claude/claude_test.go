package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// withHome points os.UserHomeDir at a temp dir holding a fake projects tree.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows
	proj := filepath.Join(home, ".claude", "projects", "-Users-x-dev-app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "sess1.jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	return proj
}

func TestCaptureCollectsJSONLOnly(t *testing.T) {
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
	if a.Source != capture.SourceClaudeCode {
		t.Errorf("Source = %q, want %q", a.Source, capture.SourceClaudeCode)
	}
	want := "claude-code/projects/-Users-x-dev-app/sess1.jsonl"
	if a.Path != want {
		t.Errorf("Path = %q, want %q", a.Path, want)
	}
}

func TestCaptureWindowFiltersOld(t *testing.T) {
	proj := withHome(t)
	old := filepath.Join(proj, "old.jsonl")
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
	// sess1.jsonl is fresh, old.jsonl is outside the 1-day window.
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (old one filtered out)", len(arts))
	}
}
