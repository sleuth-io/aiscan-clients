package cli

import (
	"os"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/buildinfo"
)

// Reusable ANSI styling for all CLI output. Dependency-free: one place owns
// TTY/NO_COLOR detection, and every command styles via the helpers below. When
// the tray/status TUI arrives (layouts, tables, adaptive themes), switch that
// to a real styling library — plain colored lines don't need one.

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	// sx's "blue" accent (#60a5fa, Tailwind blue-400) as a 24-bit truecolor code.
	ansiBlue = "\x1b[38;2;96;165;250m"
)

// colorEnabled reports whether to emit ANSI codes: only when stdout is a
// terminal and NO_COLOR is unset (https://no-color.org).
func colorEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Semantic helpers — prefer these over raw codes so meaning stays consistent.
func bold(s string) string    { return style(ansiBold, s) }
func dim(s string) string     { return style(ansiDim, s) }
func success(s string) string { return style(ansiGreen, s) }
func errorf(s string) string  { return style(ansiRed, s) }
func warn(s string) string    { return style(ansiYellow, s) }
func header(s string) string  { return style(ansiCyan, s) }
func accent(s string) string  { return style(ansiBlue, s) } // sx's light-blue accent

func style(code, s string) string {
	if !colorEnabled() {
		return s
	}
	return code + s + ansiReset
}

// VersionString renders the build version with the number emphasized and the
// commit/date metadata dimmed, e.g. "v1.2.3 (commit: abc1234, built: 2026-06-26)".
func VersionString() string {
	return accent(buildinfo.Version) + " " +
		dim("(commit: "+buildinfo.Commit+", built: "+buildinfo.Date+")")
}
