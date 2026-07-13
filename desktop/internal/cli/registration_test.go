package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaimRegistrationBounce(t *testing.T) {
	t.Setenv("AISCAN_CACHE_DIR", t.TempDir())

	if !claimRegistrationBounce("0.3.0") {
		t.Fatal("first start of a version: want bounce")
	}
	if claimRegistrationBounce("0.3.0") {
		t.Fatal("second start of the same version: want no bounce")
	}
	if !claimRegistrationBounce("0.3.1") {
		t.Fatal("first start of the next version: want bounce again")
	}
	if claimRegistrationBounce("0.3.1") {
		t.Fatal("stamp not rewritten for the new version")
	}
}

func TestClaimRegistrationBounce_UnwritableStamp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AISCAN_CACHE_DIR", dir)
	// A directory where the stamp file should be makes the write fail; the
	// claim must then read as "don't bounce" so a bounce can never loop.
	if err := os.MkdirAll(filepath.Join(dir, "registration-bounce"), 0o755); err != nil {
		t.Fatal(err)
	}
	if claimRegistrationBounce("0.3.0") {
		t.Fatal("unwritable stamp: want no bounce")
	}
}

func TestParseLaunchdPid(t *testing.T) {
	out := []byte(`gui/501/io.sleuth.aiscan = {
	active count = 1
	path = /Users/x/Library/LaunchAgents/io.sleuth.aiscan.plist
	state = running

	pid = 26430
	program = /Applications/Aiscan.app/Contents/MacOS/aiscan
}`)
	if got := parseLaunchdPid(out); got != 26430 {
		t.Fatalf("parseLaunchdPid = %d, want 26430", got)
	}
	if got := parseLaunchdPid([]byte("state = not running\n")); got != 0 {
		t.Fatalf("no pid line: parseLaunchdPid = %d, want 0", got)
	}
}
