package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CachedToken returns the cached access token for instanceURL if one exists
// and has not expired. Unlike EnsureToken it never starts a device-code flow —
// the daemon uses it to decide whether it *can* sync without interrupting the
// user with a browser prompt.
func CachedToken(instanceURL string) (string, bool) {
	t, _ := loadToken()
	if t.valid(strings.TrimRight(instanceURL, "/")) {
		return t.AccessToken, true
	}
	return "", false
}

// ErrNotLoggedIn is returned by Whoami when the server does not recognize the
// token (revoked, expired, or issued for another instance).
var ErrNotLoggedIn = fmt.Errorf("not logged in")

// Whoami asks the instance who the token belongs to and returns the username.
// It calls the server's session-introspection endpoint, which also accepts the
// device-flow Bearer token; a response without an authenticated user maps to
// ErrNotLoggedIn so callers can flip to a logged-out state.
func Whoami(ctx context.Context, instanceURL, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	endpoint := strings.TrimRight(instanceURL, "/") + "/_api/whoami/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return "", ErrNotLoggedIn
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whoami failed (%d)", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	var out struct {
		IsAuthenticated *bool  `json:"isAuthenticated"`
		Username        string `json:"username"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("whoami: bad response: %w", err)
	}
	if out.IsAuthenticated != nil && !*out.IsAuthenticated {
		return "", ErrNotLoggedIn
	}
	if out.Username == "" {
		return "", fmt.Errorf("whoami: no username in response")
	}
	return out.Username, nil
}
