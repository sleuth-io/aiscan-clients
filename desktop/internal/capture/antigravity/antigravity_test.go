package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// writeConversation lays down a fake conversation under <home>/.gemini/<dir>,
// with both transcripts plus non-transcript files that must be ignored, and
// returns the conversation's logs dir.
func writeConversation(t *testing.T, home, dir, convID string) string {
	t.Helper()
	root := filepath.Join(home, ".gemini", dir)
	logs := filepath.Join(root, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"transcript.jsonl", "transcript_full.jsonl"} {
		if err := os.WriteFile(filepath.Join(logs, name), []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-transcript artifacts that must be ignored.
	if err := os.WriteFile(filepath.Join(logs, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp_config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return logs
}

// withHome points os.UserHomeDir at a fresh temp dir and returns it.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows
	return home
}

func paths(arts []capture.Artifact) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.Path
	}
	return out
}

func TestCLICapturesJSONLOnly(t *testing.T) {
	home := withHome(t)
	writeConversation(t, home, "antigravity-cli", "abc-123")

	if !cli.detect() {
		t.Fatal("cli.detect() = false, want true")
	}
	arts, err := cli.capture(context.Background(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Both transcripts, and only the transcripts — not the .txt or .json.
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want 2 (the two .jsonl transcripts); got %v", len(arts), paths(arts))
	}
	for _, a := range arts {
		if a.Source != capture.SourceAntigravityCLI {
			t.Errorf("Source = %q, want %q", a.Source, capture.SourceAntigravityCLI)
		}
	}
	want := "antigravity-cli/brain/abc-123/.system_generated/logs/transcript_full.jsonl"
	if !contains(paths(arts), want) {
		t.Errorf("no artifact with Path %q; got %v", want, paths(arts))
	}
}

// The IDE source spans both the legacy (~/.gemini/antigravity) and the 2.0
// (~/.gemini/antigravity-ide) roots, and tags them all as antigravity-ide.
func TestIDESpansLegacyAndNewRoots(t *testing.T) {
	home := withHome(t)
	writeConversation(t, home, "antigravity", "legacy-1")
	writeConversation(t, home, "antigravity-ide", "new-1")

	if !ide.detect() {
		t.Fatal("ide.detect() = false, want true")
	}
	arts, err := ide.capture(context.Background(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Two transcripts per conversation, two conversations.
	if len(arts) != 4 {
		t.Fatalf("got %d artifacts, want 4; got %v", len(arts), paths(arts))
	}
	for _, a := range arts {
		if a.Source != capture.SourceAntigravityIDE {
			t.Errorf("Source = %q, want %q", a.Source, capture.SourceAntigravityIDE)
		}
	}
	for _, want := range []string{
		"antigravity-ide/brain/legacy-1/.system_generated/logs/transcript.jsonl",
		"antigravity-ide/brain/new-1/.system_generated/logs/transcript.jsonl",
	} {
		if !contains(paths(arts), want) {
			t.Errorf("no artifact with Path %q; got %v", want, paths(arts))
		}
	}
}

// The CLI source must not pick up IDE data and vice versa.
func TestSourcesDoNotCrossContaminate(t *testing.T) {
	home := withHome(t)
	writeConversation(t, home, "antigravity-ide", "ide-only")

	if cli.detect() {
		t.Error("cli.detect() = true with only IDE data present, want false")
	}
	arts, err := cli.capture(context.Background(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 0 {
		t.Fatalf("cli.capture picked up %d artifacts from IDE data, want 0: %v", len(arts), paths(arts))
	}
}

func TestCaptureWindowFiltersOld(t *testing.T) {
	home := withHome(t)
	logs := writeConversation(t, home, "antigravity-cli", "abc-123")
	old := filepath.Join(logs, "old.jsonl")
	if err := os.WriteFile(old, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	arts, err := cli.capture(context.Background(), capture.Options{Since: time.Now().AddDate(0, 0, -1)})
	if err != nil {
		t.Fatal(err)
	}
	// The two fresh transcripts stay; old.jsonl is outside the 1-day window.
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want 2 (old one filtered out); got %v", len(arts), paths(arts))
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
