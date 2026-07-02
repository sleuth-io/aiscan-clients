package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/autoupdate"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/buildinfo"
)

// Update implements `aiscan update`: check GitHub releases (desktop-v* tags)
// for a newer build and install it in place, sha256-verified against the
// release's checksums.txt. This explicit path works even when the daily
// background updater is disabled via AISCAN_DISABLE_AUTOUPDATER.
func Update(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "only check for a newer version, don't install")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Update aiscan to the latest release, verified against the release checksums.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan update [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--check", 19)), "only check for a newer version, don't install")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, dim("Set AISCAN_DISABLE_AUTOUPDATER=1 to turn off the daily background update."))
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	current := buildinfo.Version
	if current == "dev" || current == "" {
		return errors.New("development builds can't self-update; install from a release")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	updater, err := autoupdate.NewUpdater()
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}
	latest, found, err := updater.DetectLatest(ctx, autoupdate.Repository())
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}
	if !found {
		fmt.Fprintln(os.Stdout, "no desktop releases found")
		return nil
	}
	if latest.LessOrEqual(current) {
		fmt.Fprintf(os.Stdout, "%s already on the latest version (%s)\n", success("✓"), accent(current))
		return nil
	}

	if *checkOnly {
		fmt.Fprintf(os.Stdout, "new version available: %s (current: %s)\n", accent(latest.Version()), current)
		fmt.Fprintf(os.Stdout, "run %s to install it\n", accent("aiscan update"))
		return nil
	}

	fmt.Fprintf(os.Stdout, "updating %s → %s ...\n", current, accent(latest.Version()))
	if _, err := updater.UpdateSelf(ctx, current, autoupdate.Repository()); err != nil {
		return fmt.Errorf("update: %w", err)
	}

	// A stale background marker must not re-apply an older version next run.
	autoupdate.ClearPendingUpdate()

	fmt.Fprintf(os.Stdout, "%s updated to %s\n", success("✓"), accent(latest.Version()))
	return nil
}
