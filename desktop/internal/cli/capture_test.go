package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

func TestWriteArtifactsPreservesPaths(t *testing.T) {
	dir := t.TempDir()
	arts := []capture.Artifact{
		{Source: "claude-code", Path: "claude-code/projects/app/sess.jsonl", Data: []byte("hello")},
	}
	if err := writeArtifacts(dir, arts); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "claude-code", "projects", "app", "sess.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}
