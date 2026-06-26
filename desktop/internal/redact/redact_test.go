package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

func redactString(s string) string {
	out, _ := Redact([]capture.Artifact{{Data: []byte(s)}})
	return string(out[0].Data)
}

func TestRedactsSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		gone string // substring that must NOT remain
	}{
		{"sk key", `sk-abcdef0123456789ABCDEF`, "sk-abcdef0123456789ABCDEF"},
		{"github pat", `ghp_0123456789abcdefABCDEF0123456789abcd`, "ghp_0123456789"},
		{"aws", `AKIAIOSFODNN7EXAMPLE`, "AKIAIOSFODNN7EXAMPLE"},
		{"slack", `xoxb-1234567890-abcdefghij`, "xoxb-1234567890"},
		{"bearer", `Authorization: Bearer abcdefghijklmnopqrstuvwx`, "abcdefghijklmnopqrstuvwx"},
		{"json secret", `{"API_KEY":"supersecretvalue"}`, "supersecretvalue"},
		{"env secret", `export DB_PASSWORD=hunter2hunter2`, "hunter2hunter2"},
		{"email", `ping jane.doe+test@example.co.uk now`, "jane.doe+test@example.co.uk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactString(c.in)
			if strings.Contains(got, c.gone) {
				t.Errorf("secret survived: %q still in %q", c.gone, got)
			}
			if !strings.Contains(got, "[REDACTED") {
				t.Errorf("no placeholder emitted: %q", got)
			}
		})
	}
}

func TestPrivateKeyBlockRemoved(t *testing.T) {
	in := "before -----BEGIN RSA PRIVATE KEY-----\nMIIabc...\n-----END RSA PRIVATE KEY----- after"
	got := redactString(in)
	if strings.Contains(got, "MIIabc") {
		t.Fatalf("private key body survived: %q", got)
	}
	if !strings.Contains(got, "before ") || !strings.Contains(got, " after") {
		t.Errorf("surrounding text mangled: %q", got)
	}
}

func TestKeepsNonSecrets(t *testing.T) {
	// A plain transcript line with no secrets must be byte-identical.
	in := `{"type":"user","message":{"content":"fix the pagination bug in app.go"}}`
	if got := redactString(in); got != in {
		t.Errorf("non-secret content changed:\n in: %s\nout: %s", in, got)
	}
}

func TestRedactedJSONStaysValid(t *testing.T) {
	in := `{"role":"user","API_TOKEN":"sk-shouldbestripped123456","note":"keep me"}`
	got := redactString(in)
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n%s", err, got)
	}
	if v["note"] != "keep me" {
		t.Errorf("non-secret field lost: %v", v)
	}
}

func TestDoesNotMutateInput(t *testing.T) {
	orig := []byte(`sk-abcdef0123456789ABCDEF`)
	in := []capture.Artifact{{Data: orig}}
	_, _ = Redact(in)
	if string(orig) != `sk-abcdef0123456789ABCDEF` {
		t.Errorf("input bytes were mutated: %s", orig)
	}
}

func TestSummaryCountsAndApplied(t *testing.T) {
	in := []capture.Artifact{
		{Data: []byte(`sk-abcdef0123456789ABCDEF and a@b.com`)},
		{Data: []byte(`another c@d.com here`)},
	}
	_, sum := Redact(in)
	if sum.RulesetVersion != RulesetVersion {
		t.Errorf("ruleset = %q, want %q", sum.RulesetVersion, RulesetVersion)
	}
	if sum.Counts["email"] != 2 {
		t.Errorf("email count = %d, want 2", sum.Counts["email"])
	}
	if sum.Counts["sk-key"] != 1 {
		t.Errorf("sk-key count = %d, want 1", sum.Counts["sk-key"])
	}
	if sum.Total() != 3 {
		t.Errorf("total = %d, want 3", sum.Total())
	}
	// Applied is sorted rule names that fired.
	got := sum.Applied()
	if len(got) != 2 || got[0] != "email" || got[1] != "sk-key" {
		t.Errorf("applied = %v, want [email sk-key]", got)
	}
}
