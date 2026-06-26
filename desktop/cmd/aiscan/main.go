// Command aiscan is the desktop client: it captures local AI-tool usage, redacts
// it, and uploads it to the aiscan server for analysis. It also runs as a
// background agent with a system-tray UI and keeps itself up to date.
//
// The `login`, `capture`, and `run` verbs are implemented; daemon/tray and
// self-update are to follow.
package main

import (
	"fmt"
	"os"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/cli"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, cli.Help())
		os.Exit(2)
	}
	switch os.Args[1] {
	case "capture":
		if err := cli.Capture(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "login":
		if err := cli.Login(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "run":
		if err := cli.Run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println(cli.VersionString())
	case "help", "-h", "--help":
		fmt.Println(cli.Help())
	default:
		fmt.Fprintln(os.Stderr, cli.Help())
		os.Exit(2)
	}
}
