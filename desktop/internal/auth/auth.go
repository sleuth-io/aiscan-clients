// Package auth implements the device-code OAuth flow the desktop client uses to
// obtain a per-user access token for uploads. There are no embedded credentials:
// the client is the public, well-known `sleuth-aiscan` client (RFC 8628), and
// the only secret the device ever holds is the short-lived access token, cached
// on disk until it (nearly) expires. This mirrors the browser extension's flow
// (extension/background.js) so both clients authorize against the same server.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// ClientID is the public, well-known device-code client registered on the
	// server. It carries no secret (an RFC 8628 public client).
	ClientID = "sleuth-aiscan"
	// Scope requested for the access token.
	Scope = "skills"
	// deviceGrantType is the RFC 8628 device_code grant type.
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
)

// requestTimeout bounds a single device-authorization or token request, so a
// stalled connection can't hang the poll loop past its overall deadline.
const requestTimeout = 30 * time.Second

// Prompt is invoked when the user must approve a device authorization. It
// receives the user code and the verification URL to visit. The CLI prints
// these (and opens the browser); tests pass nil to skip the prompt.
type Prompt func(userCode, verifyURL string)

// Token is a cached access token, scoped to the instance it was issued for.
type Token struct {
	InstanceURL string    `json:"instance_url"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// valid reports whether t is a non-expired token for instanceURL. A 60s skew
// guards against using a token that expires mid-upload.
func (t *Token) valid(instanceURL string) bool {
	return t != nil && t.InstanceURL == instanceURL && t.AccessToken != "" &&
		time.Now().Add(60*time.Second).Before(t.ExpiresAt)
}

// EnsureToken returns a valid access token for instanceURL, reusing the cached
// token when possible and otherwise running the device-code flow — prompting via
// prompt and blocking until the user approves or the request times out. The
// fresh token is cached on disk (best effort; a token we cannot persist still
// works for this run).
func EnsureToken(ctx context.Context, instanceURL string, prompt Prompt) (string, error) {
	instanceURL = strings.TrimRight(instanceURL, "/")
	if t, _ := loadToken(); t.valid(instanceURL) {
		return t.AccessToken, nil
	}

	da, err := startDeviceAuthorization(ctx, instanceURL)
	if err != nil {
		return "", err
	}
	if prompt != nil {
		prompt(da.UserCode, da.verifyURL())
	}
	tok, err := pollForToken(ctx, instanceURL, da)
	if err != nil {
		return "", err
	}

	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	_ = storeToken(&Token{
		InstanceURL: instanceURL,
		AccessToken: tok.AccessToken,
		// Store the real expiry; valid() applies the early-expiry skew so the
		// skew lives in exactly one place.
		ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
	})
	return tok.AccessToken, nil
}

// ClearToken removes any cached token for instanceURL — call after a 401 so the
// next upload re-authorizes from scratch.
func ClearToken(instanceURL string) error {
	t, _ := loadToken()
	if t == nil || t.InstanceURL != strings.TrimRight(instanceURL, "/") {
		return nil
	}
	p, err := tokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// OpenBrowser tries to open target in the user's default browser. Best effort:
// the error is returned so callers can fall back to printing the URL.
func OpenBrowser(target string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "cmd", []string{"/c", "start"}
	default:
		name = "xdg-open"
	}
	args = append(args, target)
	return exec.Command(name, args...).Start()
}

// ---------------------------------------------------------------------------
// Token cache (on disk). AISCAN_CONFIG_DIR overrides the location (used by
// tests and for non-default setups); otherwise it lives under the OS config dir.
// ---------------------------------------------------------------------------

func configDir() (string, error) {
	if d := os.Getenv("AISCAN_CONFIG_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "aiscan"), nil
}

func tokenPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "token.json"), nil
}

func loadToken() (*Token, error) {
	p, err := tokenPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, nil // corrupt cache: treat as absent, re-auth
	}
	return &t, nil
}

func storeToken(t *Token) error {
	p, err := tokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600) // owner-only: it holds a bearer token
}

// ---------------------------------------------------------------------------
// Device-code flow against the configured instance.
// ---------------------------------------------------------------------------

type deviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// verifyURL is the page the user opens to approve. Prefer the server's complete
// URI (code embedded); otherwise append user_code so the approval page can
// prefill it (RFC 8628 convention).
func (d deviceAuth) verifyURL() string {
	if d.VerificationURIComplete != "" {
		return d.VerificationURIComplete
	}
	u := d.VerificationURI
	if u != "" && d.UserCode != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "user_code=" + url.QueryEscape(d.UserCode)
	}
	return u
}

func startDeviceAuthorization(ctx context.Context, instanceURL string) (deviceAuth, error) {
	form := url.Values{"client_id": {ClientID}, "scope": {Scope}}
	body, status, err := postForm(ctx, instanceURL+"/api/oauth/device-authorization/", form)
	if err != nil {
		return deviceAuth{}, err
	}
	if status != http.StatusOK {
		return deviceAuth{}, fmt.Errorf("device authorization failed (%d): %s", status, strings.TrimSpace(string(body)))
	}
	var da deviceAuth
	if err := json.Unmarshal(body, &da); err != nil {
		return deviceAuth{}, fmt.Errorf("device authorization: bad response: %w", err)
	}
	if da.DeviceCode == "" {
		return deviceAuth{}, fmt.Errorf("device authorization: no device_code in response")
	}
	return da, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
}

func pollForToken(ctx context.Context, instanceURL string, da deviceAuth) (tokenResponse, error) {
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresIn := da.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return tokenResponse{}, ctx.Err()
		case <-time.After(interval):
		}

		form := url.Values{
			"grant_type":  {deviceGrantType},
			"device_code": {da.DeviceCode},
			"client_id":   {ClientID},
		}
		body, status, err := postForm(ctx, instanceURL+"/api/oauth/token/", form)
		if err != nil {
			return tokenResponse{}, err
		}
		var tr tokenResponse
		_ = json.Unmarshal(body, &tr)
		if status == http.StatusOK && tr.AccessToken != "" {
			return tr, nil
		}
		switch tr.Error {
		case "authorization_pending":
			// keep polling
		case "slow_down":
			interval += 5 * time.Second
		case "":
			return tokenResponse{}, fmt.Errorf("authorization failed (%d)", status)
		default:
			return tokenResponse{}, fmt.Errorf("authorization failed: %s", tr.Error)
		}
	}
	return tokenResponse{}, fmt.Errorf("authorization timed out — approve the request and try again")
}

// postForm POSTs a form-urlencoded body and returns the response body + status.
// Each request is bounded by its own timeout (derived from ctx) so a connection
// that stalls after connecting can't block a caller — notably pollForToken,
// whose deadline is only re-checked between iterations — indefinitely.
func postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, res.StatusCode, err
	}
	return body, res.StatusCode, nil
}
