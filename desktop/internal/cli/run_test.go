package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// TestUploadBatch_ReauthorizesOn401 exercises the security-relevant retry path:
// a stale token gets a 401, which clears the cache, re-runs the device-code flow,
// and retries the upload with the fresh token — succeeding the second time.
func TestUploadBatch_ReauthorizesOn401(t *testing.T) {
	t.Setenv("AISCAN_CONFIG_DIR", t.TempDir())

	var ingestCalls, tokenSeen int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/aiscan/ingest":
			n := atomic.AddInt32(&ingestCalls, 1)
			if n == 1 {
				// First attempt uses the stale token → reject it.
				if got := r.Header.Get("authorization"); got != "Bearer stale" {
					t.Errorf("first ingest auth = %q, want Bearer stale", got)
				}
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// Retry must carry the freshly minted token.
			if got := r.Header.Get("authorization"); got == "Bearer fresh" {
				atomic.StoreInt32(&tokenSeen, 1)
			}
			w.Write([]byte(`{"run":"ok"}`))
		case "/api/oauth/device-authorization/":
			json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dev", "user_code": "AB-12",
				"verification_uri": "https://example.test/activate",
				"interval":         1, "expires_in": 60,
			})
		case "/api/oauth/token/":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 3600})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	token := "stale"
	var prompted bool
	prompt := func(userCode, verifyURL string) { prompted = true } // no-op: don't open a browser

	arts := []capture.Artifact{{Source: capture.SourceClaudeCode, Path: "claude-code/projects/p/s.jsonl", Data: []byte("x\n")}}
	res, err := uploadBatch(context.Background(), srv.URL, &token, capture.SourceClaudeCode, 0, arts, prompt)
	if err != nil {
		t.Fatalf("uploadBatch: %v", err)
	}
	if res.RunID != "ok" {
		t.Errorf("RunID = %q, want ok", res.RunID)
	}
	if atomic.LoadInt32(&ingestCalls) != 2 {
		t.Errorf("ingest called %d times, want 2 (one 401 + one retry)", ingestCalls)
	}
	if atomic.LoadInt32(&tokenSeen) != 1 {
		t.Error("retry did not use the refreshed token")
	}
	if token != "fresh" {
		t.Errorf("caller token = %q, want fresh (updated for later batches)", token)
	}
	if !prompted {
		t.Error("re-auth prompt was not invoked")
	}
}
