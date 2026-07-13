// Command aiscan is the desktop client: it captures local AI-tool usage, redacts
// it, and uploads it to the aiscan server for analysis. It also runs as a
// background agent with a system-tray UI (`aiscan daemon`) and keeps itself up
// to date.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/autoupdate"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/cli"
)

func main() {
	// Resolve the executable before any binary swap — afterwards
	// /proc/self/exe points at a deleted inode.
	exe, _ := os.Executable()
	if autoupdate.ApplyPendingUpdate() {
		if darwinDaemon() {
			// The macOS daemon must not exec in place: the exec'd image loses
			// its LaunchServices registration and its tray icon never appears.
			// Hand off to a fresh process instead. If the spawn fails, run on
			// as the old image — the swapped binary is adopted at the next
			// restart; exec-in-place is no fallback here, it would strand an
			// invisible daemon.
			if autoupdate.Respawn(exe) == nil {
				return
			}
		} else {
			autoupdate.Reexec(exe)
		}
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

// darwinDaemon reports whether this invocation is the macOS daemon — the
// explicit verb or a no-argv app-bundle launch. Those runs restart by spawning
// a successor rather than exec-in-place (see Respawn).
func darwinDaemon() bool {
	return runtime.GOOS == "darwin" &&
		((len(os.Args) > 1 && os.Args[1] == "daemon") || cli.LaunchedFromAppBundle())
}
