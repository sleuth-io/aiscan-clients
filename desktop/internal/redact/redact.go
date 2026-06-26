// Package redact is the shared, source-agnostic redaction pass. It runs once
// over the artifacts from every source, so the rules live in one place and
// apply uniformly. Conservative by design: when unsure, strip.
//
// Stub: real redaction (env vars, key-shaped strings, optional file contents)
// is a follow-up ticket. For now it is a pass-through so the pipeline wires up.
package redact

import "github.com/sleuth-io/aiscan-clients/desktop/internal/capture"

// Redact returns the artifacts with sensitive content stripped. Currently a
// no-op pass-through.
func Redact(arts []capture.Artifact) []capture.Artifact {
	// TODO(SK-xxx): strip secrets before anything leaves the machine.
	return arts
}
