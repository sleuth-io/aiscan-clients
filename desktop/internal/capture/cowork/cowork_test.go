package cowork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// withConfig points os.UserConfigDir at a temp dir holding a fake Cowork sessions
// tree and returns the session directory. It mirrors a real session: the captured
// metadata + audit.jsonl alongside the things we must never collect (.audit-key,
// uploads/, outputs/, .claude/) plus a sibling skills-plugin payload.
func withConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)                                // macOS: <home>/Library/Application Support
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "x")) // linux
	t.Setenv("AppData", filepath.Join(home, "AppData"))   // windows
	t.Setenv("USERPROFILE", home)

	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(base, "Claude", sessionsDir, "acct-uuid", "org-uuid", "local_S1")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(sess)

	writeFile := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Captured: metadata sibling + audit transcript.
	writeFile(filepath.Join(parent, "local_S1.json"), `{"title":"t","model":"claude"}`)
	writeFile(filepath.Join(sess, "audit.jsonl"), `{"type":"user"}`+"\n")
	// Never captured.
	writeFile(filepath.Join(sess, ".audit-key"), "SIGNINGSECRET")
	writeFile(filepath.Join(sess, "uploads", "contract.pdf"), "user upload")
	writeFile(filepath.Join(sess, "outputs", "report.docx"), "model output")
	writeFile(filepath.Join(sess, ".claude", "state.json"), `{"work":"state"}`)
	writeFile(filepath.Join(parent, "cowork-clientdata-cache.json"), `{"cache":1}`)
	// A skill payload that contains a file with a captured name, to prove the
	// skills-plugin subtree is pruned rather than scanned.
	writeFile(filepath.Join(base, "Claude", sessionsDir, "skills-plugin", "p", "audit.jsonl"), `{"type":"user"}`+"\n")
	return sess
}

func TestCaptureCollectsMetadataAndAuditOnly(t *testing.T) {
	withConfig(t)
	if !detect() {
		t.Fatal("detect() = false, want true")
	}
	arts, err := captureSessions(context.Background(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want 2 (metadata + audit.jsonl)", len(arts))
	}

	got := map[string]bool{}
	for _, a := range arts {
		if a.Source != capture.SourceClaudeCowork {
			t.Errorf("Source = %q, want %q", a.Source, capture.SourceClaudeCowork)
		}
		got[a.Path] = true
		// Nothing sensitive should ever ride along.
		for _, bad := range []string{"audit-key", "uploads", "outputs", ".claude", "skills-plugin", "cache"} {
			if strings.Contains(a.Path, bad) {
				t.Errorf("captured forbidden path %q (contains %q)", a.Path, bad)
			}
		}
	}
	for _, want := range []string{
		"claude-cowork/local-agent-mode-sessions/acct-uuid/org-uuid/local_S1.json",
		"claude-cowork/local-agent-mode-sessions/acct-uuid/org-uuid/local_S1/audit.jsonl",
	} {
		if !got[want] {
			t.Errorf("missing expected artifact %q (got %v)", want, got)
		}
	}
}

func TestCaptureWindowFiltersOld(t *testing.T) {
	sess := withConfig(t)
	old := filepath.Join(sess, "audit.jsonl")
	past := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	arts, err := captureSessions(context.Background(), capture.Options{Since: time.Now().AddDate(0, 0, -1)})
	if err != nil {
		t.Fatal(err)
	}
	// The audit log is outside the 1-day window; only the fresh metadata remains.
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (old audit.jsonl filtered out)", len(arts))
	}
	if !strings.HasSuffix(arts[0].Path, "local_S1.json") {
		t.Errorf("kept %q, want the fresh metadata file", arts[0].Path)
	}
}

func TestDetectFalseWithoutCowork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "x"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("USERPROFILE", home)
	if detect() {
		t.Fatal("detect() = true with no Cowork install, want false")
	}
}
