package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/buildinfo"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/tray"
)

// launchAgentLabel is the launchd job label and plist basename on macOS.
const launchAgentLabel = "io.sleuth.aiscan"

// Daemon implements `aiscan daemon`: the resident agent. It syncs on an
// interval, keeps itself updated (restarting at an idle point to adopt a
// swapped binary), and renders its state through the system tray — the trust
// surface. Launching the macOS app bundle lands here too, via main's
// no-args-from-bundle detection.
func Daemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	instance := fs.String("instance", defaultInstance(), "aiscan instance URL to sync with")
	noTray := fs.Bool("no-tray", false, "run headless, without the system tray")
	interval := fs.Duration("interval", time.Hour, "time between scheduled syncs")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Run the resident agent: sync on a schedule, show status in the system")
		fmt.Fprintln(os.Stderr, "tray, and self-update. On macOS the Aiscan app is this verb in a bundle.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan daemon [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--instance URL", 19)), "aiscan instance to sync with (default "+defaultInstance()+")")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--no-tray", 19)), "run headless, without the system tray")
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--interval DUR", 19)), "time between scheduled syncs (default 1h)")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, dim("  AISCAN_INSTANCE changes the default instance (handy in the LaunchAgent"))
		fmt.Fprintln(os.Stderr, dim("  plist for pointing a machine at a test server)."))
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// Everything the daemon prints goes to a log file, so disable ANSI styling
	// globally (color.go honors NO_COLOR per call).
	_ = os.Setenv("NO_COLOR", "1")

	// Resolve once, before any update swap can replace the path's target.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	logW, logPath, err := openDaemonLog()
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	// When run interactively (testing), tee the log to stderr.
	if info, err := os.Stderr.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		logW = io.MultiWriter(logW, os.Stderr)
	}
	logger := log.New(logW, "", log.LstdFlags)

	release, locked, err := acquireDaemonLock()
	if err != nil {
		logger.Printf("single-instance lock unavailable (%v); continuing unguarded", err)
	} else if !locked {
		const msg = "aiscan is already running — look for its icon in the menu bar."
		fmt.Fprintln(os.Stderr, msg)
		if fromAppBundle(exe) {
			alert(msg)
		}
		return nil
	}
	if release != nil {
		defer release()
	}

	// A quarantined app launched straight from the disk image (or Downloads)
	// runs from a read-only randomized path: self-update can't swap the binary
	// and a LaunchAgent would record a dead path. Tell the user what to do
	// instead of half-working.
	if translocated(exe) {
		logger.Printf("launched app-translocated from %s; asking the user to move it", exe)
		alert("Move Aiscan into the Applications folder (drag it from the disk image), then open it again.")
		return nil
	}

	if fromAppBundle(exe) {
		if err := installLaunchAgent(exe); err != nil {
			logger.Printf("install launch agent: %v", err)
		}
	}

	logger.Printf("daemon starting: version=%s instance=%s interval=%s log=%s tray=%t",
		buildinfo.Version, *instance, *interval, logPath, !*noTray)

	a := newAgent(*instance, exe, *interval, logger, logW)
	// Hand the agent the lock release so a macOS update restart can pass the
	// single-instance lock to its spawned successor (agent.restart).
	a.releaseLock = release
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *noTray {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sig
			logger.Printf("signal received; stopping")
			cancel()
		}()
		a.run(ctx)
		return nil
	}

	go a.run(ctx)
	// Blocks until Quit; must stay on the main goroutine (Cocoa event loop).
	tray.Run(a, a.States(), buildinfo.Version)
	logger.Printf("daemon stopped")
	return nil
}

// LaunchedFromAppBundle reports whether this process was started from inside a
// macOS .app bundle — main uses it to route a bare double-click (no argv) to
// the daemon instead of printing CLI help nobody will see.
func LaunchedFromAppBundle() bool {
	exe, err := os.Executable()
	return err == nil && fromAppBundle(exe)
}

func fromAppBundle(exe string) bool {
	return runtime.GOOS == "darwin" && strings.Contains(exe, ".app/Contents/MacOS/")
}

// translocated reports whether macOS Gatekeeper is running us from an App
// Translocation mount (quarantined app opened in place).
func translocated(exe string) bool {
	return runtime.GOOS == "darwin" && strings.Contains(exe, "/AppTranslocation/")
}

// alert shows a blocking native dialog on macOS (the daemon has no window to
// speak through); a no-op elsewhere. Best effort — errors are ignored because
// there is no better channel to report them on.
func alert(msg string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := fmt.Sprintf("display dialog %q with title \"Aiscan\" buttons {\"OK\"} default button 1", msg)
	_ = exec.Command("osascript", "-e", script).Run()
}

// daemonLogDir is where the daemon writes its log: the conventional
// user-visible location on macOS (Console.app picks it up), the aiscan cache
// dir elsewhere.
func daemonLogDir() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Logs", "aiscan"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "aiscan", "logs"), nil
}

// openDaemonLog opens the append-only daemon log, rotating a grown one aside
// first so it can't fill the disk on a machine nobody is watching.
func openDaemonLog() (io.Writer, string, error) {
	dir, err := daemonLogDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "daemon.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 5<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// installLaunchAgent writes the launchd user agent that starts the daemon at
// login and relaunches it after a crash — but not after a clean exit, so
// "Quit" from the tray sticks (KeepAlive/SuccessfulExit=false). Update
// adoption doesn't rely on launchd at all: the daemon re-execs itself.
// Idempotent: rewrites only when content differs (e.g. the app moved).
func installLaunchAgent(exe string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logDir, err := daemonLogDir()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchAgentLabel, exe, filepath.Join(logDir, "launchd.log"), filepath.Join(logDir, "launchd.log"))

	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, launchAgentLabel+".plist")
	if existing, err := os.ReadFile(path); err == nil && string(existing) == plist {
		return nil
	}
	return os.WriteFile(path, []byte(plist), 0o644)
}
