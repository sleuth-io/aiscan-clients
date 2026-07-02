package autoupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/buildinfo"
)

// useTempCache isolates the test from the real cache directory.
func useTempCache(t *testing.T) {
	t.Helper()
	t.Setenv("AISCAN_CACHE_DIR", t.TempDir())
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	original := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = original })
	buildinfo.Version = v
}

func writeMarker(t *testing.T, content []byte) string {
	t.Helper()
	markerPath, err := pendingUpdatePath()
	if err != nil {
		t.Fatalf("pendingUpdatePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(markerPath, content, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return markerPath
}

func TestCheckAndUpdateDevBuild(t *testing.T) {
	useTempCache(t)
	setVersion(t, "dev")

	updated, err := checkAndUpdate()
	if err != nil {
		t.Errorf("expected dev build to be a silent no-op, got: %v", err)
	}
	if updated {
		t.Error("expected dev build to never report an update")
	}
}

func TestShouldCheckWithNoCache(t *testing.T) {
	useTempCache(t)

	if !shouldCheck() {
		t.Error("expected shouldCheck to be true when no throttle file exists")
	}
}

func TestShouldCheckWithRecentCache(t *testing.T) {
	useTempCache(t)

	if err := updateCheckTimestamp(); err != nil {
		t.Fatalf("updateCheckTimestamp: %v", err)
	}
	if shouldCheck() {
		t.Error("expected shouldCheck to be false right after a check")
	}
}

func TestShouldCheckWithOldCache(t *testing.T) {
	useTempCache(t)

	if err := updateCheckTimestamp(); err != nil {
		t.Fatalf("updateCheckTimestamp: %v", err)
	}
	throttle := filepath.Join(os.Getenv("AISCAN_CACHE_DIR"), updateCacheFile)
	oldTime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(throttle, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !shouldCheck() {
		t.Error("expected shouldCheck to be true once past the check interval")
	}
}

func TestClearPendingUpdate(t *testing.T) {
	useTempCache(t)
	markerPath := writeMarker(t, []byte(`{"version":"1.0.0"}`))

	ClearPendingUpdate()

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Error("marker should be gone after ClearPendingUpdate")
	}
}

func TestClearPendingUpdateNoFile(t *testing.T) {
	useTempCache(t)
	ClearPendingUpdate()
}

func TestApplyPendingUpdateNoMarker(t *testing.T) {
	useTempCache(t)
	setVersion(t, "0.10.0")

	if ApplyPendingUpdate() {
		t.Error("expected false when no marker exists")
	}
}

func TestApplyPendingUpdateDevBuild(t *testing.T) {
	useTempCache(t)
	setVersion(t, "dev")
	markerPath := writeMarker(t, []byte(`{"version":"1.0.0"}`))

	if ApplyPendingUpdate() {
		t.Error("expected false for dev build")
	}
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		t.Error("marker should survive a dev-build skip")
	}
}

func TestApplyPendingUpdateDirtyBuild(t *testing.T) {
	useTempCache(t)
	setVersion(t, "0.12.6-6-g3f51665-dirty")
	markerPath := writeMarker(t, []byte(`{"version":"1.0.0"}`))

	if ApplyPendingUpdate() {
		t.Error("expected false for dirty build")
	}
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		t.Error("marker should survive a dirty-build skip")
	}
}

func TestApplyPendingUpdateDisabled(t *testing.T) {
	useTempCache(t)
	setVersion(t, "0.10.0")
	t.Setenv("AISCAN_DISABLE_AUTOUPDATER", "1")
	markerPath := writeMarker(t, []byte(`{"version":"1.0.0"}`))

	if ApplyPendingUpdate() {
		t.Error("expected false when disabled")
	}
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		t.Error("marker should survive when the autoupdater is disabled")
	}
}

func TestApplyPendingUpdateAlreadyUpToDate(t *testing.T) {
	useTempCache(t)
	setVersion(t, "2.0.0")

	data, _ := json.Marshal(pendingUpdate{
		Version:   "1.0.0",
		AssetURL:  "https://example.com/asset.tar.gz",
		AssetName: "asset.tar.gz",
	})
	markerPath := writeMarker(t, data)

	if ApplyPendingUpdate() {
		t.Error("expected false when already at or ahead of the pending version")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Error("marker should be removed when already up to date")
	}
}

func TestApplyPendingUpdateInvalidJSON(t *testing.T) {
	useTempCache(t)
	setVersion(t, "0.10.0")
	markerPath := writeMarker(t, []byte(`not json`))

	if ApplyPendingUpdate() {
		t.Error("expected false for an unreadable marker")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Error("invalid marker should be removed")
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	useTempCache(t)

	pending := pendingUpdate{
		Version:   "1.2.3",
		AssetURL:  "https://github.com/sleuth-io/aiscan-clients/releases/download/desktop-v1.2.3/aiscan_Linux_x86_64.tar.gz",
		AssetName: "aiscan_Linux_x86_64.tar.gz",
	}
	data, err := json.Marshal(pending)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	markerPath := writeMarker(t, data)

	readData, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got pendingUpdate
	if err := json.Unmarshal(readData, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != pending {
		t.Errorf("round trip = %+v, want %+v", got, pending)
	}
}
