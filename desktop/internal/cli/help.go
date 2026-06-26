package cli

import (
	"fmt"
	"strings"
)

// Help returns the colored top-level usage text, modeled on the sx CLI layout:
// a description, the version line, then cyan section headers with bold command
// names. Colors degrade automatically (see color.go).
func Help() string {
	var b strings.Builder
	fmt.Fprintln(&b, bold("aiscan")+" — capture local AI-tool usage, redact it, and upload for analysis")
	fmt.Fprintln(&b, VersionString())
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, header("Usage:"))
	fmt.Fprintln(&b, "  "+accent("aiscan <command> [flags]"))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, header("Available Commands:"))
	fmt.Fprintf(&b, "  %s %s\n", bold(rpad("login", 9)), "Authorize this machine (device-code OAuth)")
	fmt.Fprintf(&b, "  %s %s\n", bold(rpad("capture", 9)), "Collect local AI usage (--out DIR, --window-days N)")
	fmt.Fprintf(&b, "  %s %s\n", bold(rpad("run", 9)), "Capture, redact, and upload for analysis (--instance, --window-days N)")
	fmt.Fprintf(&b, "  %s %s\n", bold(rpad("version", 9)), "Print version information")
	fmt.Fprintf(&b, "  %s %s\n", bold(rpad("help", 9)), "Show this help")
	fmt.Fprintln(&b)
	fmt.Fprint(&b, dim(`Run "aiscan <command> -h" for command flags.`))
	return b.String()
}

// rpad right-pads s with spaces to width n. Pad the plain text before coloring
// so ANSI codes don't throw off the alignment.
func rpad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
