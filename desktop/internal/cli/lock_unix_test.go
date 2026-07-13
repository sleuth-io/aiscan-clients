//go:build !windows

package cli

import (
	"os"
	"testing"
)

func TestDaemonLockRecordsPid(t *testing.T) {
	t.Setenv("AISCAN_CACHE_DIR", t.TempDir())

	release, locked, err := acquireDaemonLock()
	if err != nil || !locked {
		t.Fatalf("acquireDaemonLock: locked=%v err=%v", locked, err)
	}
	defer release()

	if got := readDaemonLockPid(); got != os.Getpid() {
		t.Fatalf("lock pid = %d, want %d", got, os.Getpid())
	}

	// A second acquire while held must report locked=false (flock treats a
	// separate fd as a separate owner, so this holds within one process).
	if _, locked, err := acquireDaemonLock(); err != nil || locked {
		t.Fatalf("second acquire: locked=%v err=%v, want false, nil", locked, err)
	}
}

func TestReadDaemonLockPid_NoneRecorded(t *testing.T) {
	t.Setenv("AISCAN_CACHE_DIR", t.TempDir())

	if got := readDaemonLockPid(); got != 0 {
		t.Fatalf("no lock file: pid = %d, want 0", got)
	}
	// A pre-pid-recording daemon leaves the file empty.
	path, err := daemonLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readDaemonLockPid(); got != 0 {
		t.Fatalf("empty lock file: pid = %d, want 0", got)
	}
}
