package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/auth"
)

// Login implements `aiscan login`: authorize this machine against an aiscan
// instance via the device-code flow and cache the resulting token, so a later
// `aiscan sync` uploads without an interactive step. (sync also authorizes on
// first use, so login is optional — it just front-loads the approval.)
func Login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	instance := fs.String("instance", defaultInstance, "aiscan instance URL to authorize against")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Authorize this machine against an aiscan instance (device-code OAuth)")
		fmt.Fprintln(os.Stderr, "and cache the token for later uploads.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Usage:"))
		fmt.Fprintln(os.Stderr, "  "+accent("aiscan login [flags]"))
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, header("Flags:"))
		fmt.Fprintf(os.Stderr, "  %s %s\n", accent(rpad("--instance URL", 19)), "aiscan instance to authorize against (default "+defaultInstance+")")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if _, err := auth.EnsureToken(context.Background(), *instance, devicePrompt(os.Stdout)); err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	fmt.Fprintf(os.Stdout, "%s authorized for %s\n", success("✓"), accent(strings.TrimRight(*instance, "/")))
	return nil
}
