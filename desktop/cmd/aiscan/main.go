// Command aiscan is the desktop client: it captures local AI-tool usage, redacts
// it, and uploads it to the aiscan server for analysis. It also runs as a
// background agent with a system-tray UI (`aiscan daemon`) and keeps itself up
// to date.
package main

import (
	"fmt"
	"os"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/autoupdate"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/cli"
)

func main() {
	// Resolve the executable before any binary swap — afterwards
	// /proc/self/exe points at a deleted inode.
	exe, _ := os.Executable()
	if autoupdate.ApplyPendingUpdate() {
		autoupdate.Reexec(exe)
	}
	// Daily background check, unless the user is updating explicitly.
	if len(os.Args) < 2 || os.Args[1] != "update" {
		autoupdate.CheckAndUpdateInBackground()
	}

	if len(os.Args) < 2 {
		// A double-clicked macOS app bundle launches with no argv; that user
		// wants the tray agent, not CLI help printed into the void.
		if cli.LaunchedFromAppBundle() {
			if err := cli.Daemon(nil); err != nil {
				fmt.Fprintln(os.Stderr, cli.ErrorPrefix(), err)
				os.Exit(1)
			}
			return
		}
		fmt.Fprintln(os.Stderr, cli.Help())
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		if err := cli.Daemon(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, cli.ErrorPrefix(), err)
			os.Exit(1)
		}
	case "capture":
		if err := cli.Capture(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, cli.ErrorPrefix(), err)
			os.Exit(1)
		}
	case "login":
		if err := cli.Login(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, cli.ErrorPrefix(), err)
			os.Exit(1)
		}
	case "sync":
		if err := cli.Sync(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, cli.ErrorPrefix(), err)
			os.Exit(1)
		}
	case "update":
		if err := cli.Update(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, cli.ErrorPrefix(), err)
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
