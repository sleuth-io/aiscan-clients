package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withTempConfig points the token cache at a temp dir for the duration of a test.
func withTempConfig(t *testing.T) {
	t.Helper()
	t.Setenv("AISCAN_CONFIG_DIR", t.TempDir())
}

func TestTokenValid(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	cases := []struct {
		name string
		tok  *Token
		inst string
		want bool
	}{
		{"nil", nil, "https://x", false},
		{"ok", &Token{InstanceURL: "https://x", AccessToken: "t", ExpiresAt: future}, "https://x", true},
		{"expired", &Token{InstanceURL: "https://x", AccessToken: "t", ExpiresAt: past}, "https://x", false},
		{"wrong instance", &Token{InstanceURL: "https://y", AccessToken: "t", ExpiresAt: future}, "https://x", false},
		{"no token", &Token{InstanceURL: "https://x", ExpiresAt: future}, "https://x", false},
	}
	for _, c := range cases {
		if got := c.tok.valid(c.inst); got != c.want {
			t.Errorf("%s: valid = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEnsureToken_UsesCache(t *testing.T) {
	withTempConfig(t)
	if err := storeToken(&Token{InstanceURL: "https://cached", AccessToken: "cached-tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	// No server: a cache hit must not make any network call.
	got, err := EnsureToken(context.Background(), "https://cached/", nil)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if got != "cached-tok" {
		t.Fatalf("token = %q, want cached-tok", got)
	}
}

func TestEnsureToken_DeviceFlow(t *testing.T) {
	withTempConfig(t)

	var prompted bool
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/device-authorization/":
			_ = r.ParseForm()
			if r.Form.Get("client_id") != ClientID || r.Form.Get("scope") != Scope {
				t.Errorf("device-auth form = %v", r.Form)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-code",
				"user_code":        "WXYZ-1234",
				"verification_uri": "https://example.test/activate",
				"interval":         1, // 1s keeps the poll loop fast
				"expires_in":       60,
			})
		case "/api/oauth/token/":
			_ = r.ParseForm()
			if r.Form.Get("device_code") != "dev-code" || r.Form.Get("grant_type") != deviceGrantType {
				t.Errorf("token form = %v", r.Form)
			}
			polls++
			if polls == 1 {
				json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-tok", "expires_in": 3600})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	prompt := func(userCode, verifyURL string) {
		prompted = true
		if userCode != "WXYZ-1234" {
			t.Errorf("userCode = %q", userCode)
		}
		// verifyURL synthesizes ?user_code= from the plain verification_uri.
		if !strings.Contains(verifyURL, "user_code=WXYZ-1234") {
			t.Errorf("verifyURL = %q", verifyURL)
		}
	}

	got, err := EnsureToken(context.Background(), srv.URL, prompt)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if got != "fresh-tok" {
		t.Fatalf("token = %q, want fresh-tok", got)
	}
	if !prompted {
		t.Error("prompt was not called")
	}
	if polls < 2 {
		t.Errorf("expected to poll past authorization_pending, polls = %d", polls)
	}

	// The fresh token should now be cached for reuse.
	cached, _ := loadToken()
	if !cached.valid(srv.URL) {
		t.Errorf("token was not cached: %#v", cached)
	}
}

func TestClearToken(t *testing.T) {
	withTempConfig(t)
	if err := storeToken(&Token{InstanceURL: "https://x", AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	if err := ClearToken("https://x/"); err != nil {
		t.Fatalf("ClearToken: %v", err)
	}
	if tok, _ := loadToken(); tok != nil {
		t.Errorf("token still present after clear: %#v", tok)
	}
	// Clearing again (or an unknown instance) is a no-op, not an error.
	if err := ClearToken("https://x"); err != nil {
		t.Errorf("ClearToken on missing: %v", err)
	}
}

func TestPollForToken_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the first poll tick

	// The address is never dialed: the poll loop selects ctx.Done() first.
	_, err := pollForToken(ctx, "http://127.0.0.1:0", deviceAuth{DeviceCode: "d", Interval: 1, ExpiresIn: 60})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestVerifyURL_PrefersComplete(t *testing.T) {
	d := deviceAuth{VerificationURIComplete: "https://x/activate?user_code=AB", UserCode: "AB", VerificationURI: "https://x/activate"}
	if got := d.verifyURL(); got != "https://x/activate?user_code=AB" {
		t.Errorf("verifyURL = %q", got)
	}
	d2 := deviceAuth{VerificationURI: "https://x/activate?foo=1", UserCode: "AB"}
	if got := d2.verifyURL(); got != "https://x/activate?foo=1&user_code=AB" {
		t.Errorf("verifyURL = %q", got)
	}
}
