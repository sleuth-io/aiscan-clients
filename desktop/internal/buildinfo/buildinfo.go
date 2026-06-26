// Package buildinfo holds version metadata stamped into the binary at build
// time via -ldflags (see the Makefile). Defaults apply to plain `go build`/
// `go run`, where no ldflags are set.
package buildinfo

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders the one-line version summary, matching the sx client's format:
// "v1.2.3 (commit: abc1234, built: 2026-06-26)".
func String() string {
	return Version + " (commit: " + Commit + ", built: " + Date + ")"
}
