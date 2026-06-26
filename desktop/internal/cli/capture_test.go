package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// secretArts is a fixture with one obvious secret per artifact.
func secretArts() []capture.Artifact {
	return []capture.Artifact{
		{Source: "claude-code", Path: "claude-code/projects/app/a.jsonl",
			Data: []byte(`{"msg":"key is sk-abcdef0123456789ABCDEF ok"}`)},
		{Source: "claude-code", Path: "claude-code/projects/web/b.jsonl",
			Data: []byte(`{"msg":"mail me at jane@example.com"}`)},
	}
}

// TestProcessRedactsByDefault is the core contract: redaction runs before
// anything is written, so the on-disk dump must not contain the raw secrets.
func TestProcessRedactsByDefault(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := (captureRun{out: dir}).process(&buf, secretArts()); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "claude-code", "projects", "app", "a.jsonl"))
	b, _ := os.ReadFile(filepath.Join(dir, "claude-code", "projects", "web", "b.jsonl"))
	if strings.Contains(string(a), "sk-abcdef0123456789ABCDEF") {
		t.Errorf("sk- key written to disk unredacted: %s", a)
	}
	if strings.Contains(string(b), "jane@example.com") {
		t.Errorf("email written to disk unredacted: %s", b)
	}
	if !strings.Contains(string(a), "[REDACTED") {
		t.Errorf("no placeholder in written file: %s", a)
	}
	// The summary line reports what was stripped.
	if out := buf.String(); !strings.Contains(out, "redacted:") {
		t.Errorf("no redaction summary printed: %q", out)
	}
}

// TestProcessNoRedactKeepsRawBytes: --no-redact is debug-only and must write the
// captured bytes verbatim, with the summary flagging that the gate was skipped.
func TestProcessNoRedactKeepsRawBytes(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := (captureRun{out: dir, noRedact: true}).process(&buf, secretArts()); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "claude-code", "projects", "app", "a.jsonl"))
	if !strings.Contains(string(a), "sk-abcdef0123456789ABCDEF") {
		t.Errorf("--no-redact should keep raw bytes, got: %s", a)
	}
	if out := buf.String(); !strings.Contains(out, "skipped") {
		t.Errorf("summary should flag skipped redaction: %q", out)
	}
}

// TestProcessSummaryCounts: the trust-surface line names each rule that fired
// with its count.
func TestProcessSummaryCounts(t *testing.T) {
	var buf bytes.Buffer
	if err := (captureRun{}).process(&buf, secretArts()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"redacted:", "sk-key 1", "email 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q in:\n%s", want, out)
		}
	}
}

// TestProcessNothingMatched: clean input reports nothing matched and writes the
// bytes unchanged.
func TestProcessNothingMatched(t *testing.T) {
	var buf bytes.Buffer
	arts := []capture.Artifact{{Source: "claude-code", Path: "p/a.jsonl", Data: []byte(`{"msg":"fix the bug"}`)}}
	if err := (captureRun{}).process(&buf, arts); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "nothing matched") {
		t.Errorf("expected 'nothing matched', got: %q", out)
	}
}

// TestProcessShowRedactions: the debug view lists each match tagged with the
// artifact it came from.
func TestProcessShowRedactions(t *testing.T) {
	var buf bytes.Buffer
	if err := (captureRun{showRedactions: true}).process(&buf, secretArts()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "claude-code/projects/app/a.jsonl") {
		t.Errorf("show-redactions should tag matches with their source path:\n%s", out)
	}
}

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
