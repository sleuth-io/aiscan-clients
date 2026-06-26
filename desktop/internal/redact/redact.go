// Package redact is the shared, source-agnostic redaction pass. It runs once
// over the artifacts from every source, so the rules live in one place and
// apply uniformly. Conservative by design: it targets obvious *secrets* (keys,
// tokens, private keys), not general PII — when unsure, strip.
//
// Rules are deliberately stable and rarely change: a churny redactor would
// reintroduce the fat-client problem the thin client exists to avoid. Bump
// RulesetVersion on any rule change so uploads stay auditable.
package redact

import (
	"regexp"
	"sort"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// RulesetVersion identifies the rule set applied, for the upload envelope's
// redaction.ruleset_version field (protocol/upload-request.schema.json).
const RulesetVersion = "1"

const placeholder = "[REDACTED]"

type rule struct {
	name string
	re   *regexp.Regexp
	repl string
}

// rules run in order over each artifact's raw bytes. Replacements stay inside
// JSON string values (the token is swapped for a placeholder, quotes untouched),
// so redacted JSONL remains well-formed for the server's parser.
var rules = []rule{
	{
		// PEM private key blocks (RSA/EC/OPENSSH/...). Non-greedy so adjacent
		// blocks don't merge. [\s\S] spans both real and escaped newlines.
		"private-key-block",
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		"[REDACTED PRIVATE KEY]",
	},
	{
		// OpenAI / Anthropic / Stripe style "sk-..." keys.
		"sk-key",
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
		placeholder,
	},
	{
		// GitHub personal access / OAuth / app tokens.
		"github-token",
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`),
		placeholder,
	},
	{
		"aws-access-key-id",
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		placeholder,
	},
	{
		"slack-token",
		regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
		placeholder,
	},
	{
		"google-api-key",
		regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
		placeholder,
	},
	{
		// Authorization: Bearer <token> — keep the scheme, drop the secret.
		"bearer-token",
		regexp.MustCompile(`(?i)(bearer )[A-Za-z0-9._-]{20,}`),
		"${1}" + placeholder,
	},
	{
		// Email addresses (PII). Conservative: strip them all.
		"email",
		regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
		"[REDACTED EMAIL]",
	},
	{
		// JSON: "...SECRET/TOKEN/PASSWORD/API_KEY...": "value" — keep key, drop value.
		"secret-assignment-json",
		regexp.MustCompile(`(?i)("[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|ACCESS_?KEY|CREDENTIAL|PRIVATE_?KEY)[A-Z0-9_]*"\s*:\s*)"[^"]*"`),
		`${1}"` + placeholder + `"`,
	},
	{
		// Shell/env: NAME_WITH_SECRET=value — keep name, drop value.
		"secret-assignment-env",
		regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|ACCESS_?KEY|CREDENTIAL|PRIVATE_?KEY)[A-Z0-9_]*=)[^\s"']+`),
		"${1}" + placeholder,
	},
}

// Summary records what redaction did. It feeds the trust surface (what the CLI
// and tray show the user) and the upload envelope's redaction fields
// (ruleset_version + applied) in protocol/upload-request.schema.json.
type Summary struct {
	RulesetVersion string
	Counts         map[string]int // rule name -> number of matches redacted
	Matches        []Match        // debug: every match, in order
}

// Match is one redacted hit (for the --show-redactions debug view).
type Match struct {
	Rule string
	Path string // artifact the hit came from, e.g. claude-code/projects/<proj>/<session>.jsonl
	Text string
}

// Total is the number of redactions across all rules.
func (s Summary) Total() int {
	n := 0
	for _, c := range s.Counts {
		n += c
	}
	return n
}

// Applied returns the names of rules that matched at least once, sorted — the
// value for the envelope's redaction.applied field.
func (s Summary) Applied() []string {
	names := make([]string, 0, len(s.Counts))
	for name := range s.Counts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Redact returns copies of the artifacts with secrets stripped from their bytes,
// plus a summary of what was removed. The input artifacts are not mutated.
func Redact(arts []capture.Artifact) ([]capture.Artifact, Summary) {
	sum := Summary{RulesetVersion: RulesetVersion, Counts: map[string]int{}}
	out := make([]capture.Artifact, len(arts))
	for i, a := range arts {
		data := a.Data
		for _, r := range rules {
			if m := r.re.FindAll(data, -1); len(m) > 0 {
				sum.Counts[r.name] += len(m)
				for _, hit := range m {
					sum.Matches = append(sum.Matches, Match{Rule: r.name, Path: a.Path, Text: string(hit)})
				}
				data = r.re.ReplaceAll(data, []byte(r.repl))
			}
		}
		a.Data = data
		out[i] = a
	}
	return out, sum
}
