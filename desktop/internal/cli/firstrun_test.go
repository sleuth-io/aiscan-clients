package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testInstance = "https://aiscan.example.com"

// writeCachedToken drops a valid token.json into the config dir, the same
// shape auth writes (auth.Token).
func writeCachedToken(t *testing.T, dir string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"instance_url": testInstance,
		"access_token": "tok",
		"expires_at":   time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func markerExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, "login-prompted"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestClaimFirstRunLogin_FirstRunNoToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AISCAN_CONFIG_DIR", dir)

	if !claimFirstRunLogin(testInstance) {
		t.Fatal("first run with no token: want claim=true")
	}
	if !markerExists(t, dir) {
		t.Fatal("marker not written after claim")
	}
	// A second daemon start must never prompt again.
	if claimFirstRunLogin(testInstance) {
		t.Fatal("second run: want claim=false")
	}
}

func TestClaimFirstRunLogin_AlreadyLoggedIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AISCAN_CONFIG_DIR", dir)
	writeCachedToken(t, dir)

	if claimFirstRunLogin(testInstance) {
		t.Fatal("logged in on first run: want claim=false")
	}
	// The marker must still be stamped, so a later logout sticks instead of
	// re-triggering the prompt on the next start.
	if !markerExists(t, dir) {
		t.Fatal("marker not written on an already-logged-in first run")
	}
	if claimFirstRunLogin(testInstance) {
		t.Fatal("after logout on a stamped install: want claim=false")
	}
}

func TestClaimFirstRunLogin_MarkerPreexists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AISCAN_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "login-prompted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if claimFirstRunLogin(testInstance) {
		t.Fatal("marker present: want claim=false")
	}
}
