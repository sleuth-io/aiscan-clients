// Command aiscan is the desktop client: it captures local AI-tool usage, redacts
// it, and uploads it to the aiscan server for analysis. It also runs as a
// background agent with a system-tray UI and keeps itself up to date.
//
// Only the `capture` verb is implemented so far; redact/upload/daemon/tray and
// self-update are to follow.
package main

import (
	"fmt"
	"os"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/cli"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "capture":
		if err := cli.Capture(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println(cli.VersionString())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: aiscan <command>")
	fmt.Fprintln(os.Stderr, "  capture [--out DIR] [--window-days N]   collect local AI usage")
	fmt.Fprintln(os.Stderr, "  version | -v | --version                print version")
}
