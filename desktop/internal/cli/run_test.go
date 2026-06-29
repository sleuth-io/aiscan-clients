package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/upload"
)

// TestSelectRecipes covers the --source filter: empty = all, a named subset, and
// a loud error on an unknown source (so a typo doesn't silently upload nothing).
func TestSelectRecipes(t *testing.T) {
	rs := []capture.Recipe{
		{ID: capture.SourceClaudeCode},
		{ID: capture.SourceClaudeCowork},
	}

	all, err := selectRecipes(rs, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("empty source: got %d recipes, err=%v; want all 2", len(all), err)
	}

	only, err := selectRecipes(rs, "claude-cowork")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(only) != 1 || only[0].ID != capture.SourceClaudeCowork {
		t.Fatalf("got %v, want [claude-cowork]", only)
	}

	multi, err := selectRecipes(rs, " claude-cowork , claude-code ")
	if err != nil || len(multi) != 2 {
		t.Fatalf("multi/whitespace: got %d recipes, err=%v; want 2", len(multi), err)
	}

	if _, err := selectRecipes(rs, "claude-cowork,bogus"); err == nil {
		t.Fatal("expected error for unknown source, got nil")
	}
}

// oneBatch packs arts into a single upload.Batch (failing the test if they don't
// fit in one), so tests can drive uploadBatch/uploadAdaptive with a real body.
func oneBatch(t *testing.T, arts []capture.Artifact) upload.Batch {
	t.Helper()
	batches, err := upload.SplitForUpload(arts, upload.MaxCompressedBytes)
	if err != nil {
		t.Fatalf("SplitForUpload: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(batches))
	}
	return batches[0]
}

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
	res, err := uploadBatch(context.Background(), srv.URL, &token, capture.SourceClaudeCode, 0, oneBatch(t, arts), prompt)
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

// TestUploadAdaptive_SplitsOn413 verifies the heavy-history path: a server that
// rejects any body with more than two sessions (413) drives the client to halve
// the batch until every part fits — covering all sessions across several runs.
func TestUploadAdaptive_SplitsOn413(t *testing.T) {
	var runs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/aiscan/ingest" {
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gz, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		tr := tar.NewReader(gz)
		members := 0
		for {
			if _, e := tr.Next(); e != nil {
				break
			}
			members++
		}
		if members > 2 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		n := atomic.AddInt32(&runs, 1)
		fmt.Fprintf(w, `{"run":"r%d"}`, n)
	}))
	defer srv.Close()

	arts := make([]capture.Artifact, 5)
	for i := range arts {
		arts[i] = capture.Artifact{Source: capture.SourceClaudeCode, Path: fmt.Sprintf("claude-code/projects/p/s%d.jsonl", i), Data: []byte("x\n")}
	}

	token := "tok"
	results, err := uploadAdaptive(context.Background(), srv.URL, &token, capture.SourceClaudeCode, 0, oneBatch(t, arts), func(string, string) {})
	if err != nil {
		t.Fatalf("uploadAdaptive: %v", err)
	}
	sessions := 0
	seen := map[string]bool{}
	for _, r := range results {
		sessions += r.Sessions
		if r.RunID == "" || seen[r.RunID] {
			t.Errorf("missing or duplicate run id: %#v", r)
		}
		seen[r.RunID] = true
	}
	if sessions != 5 {
		t.Errorf("uploaded %d sessions across parts, want 5", sessions)
	}
	for _, r := range results {
		if r.Sessions > 2 {
			t.Errorf("a part carried %d sessions, server cap is 2", r.Sessions)
		}
	}
}

// TestUploadAdaptive_LoneSessionTooLarge verifies that when a single session
// can't be split any further and the server still 413s, the user gets a clear
// error instead of an opaque 413.
func TestUploadAdaptive_LoneSessionTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge) // reject everything
	}))
	defer srv.Close()

	arts := []capture.Artifact{{Source: capture.SourceClaudeCode, Path: "claude-code/projects/p/s.jsonl", Data: []byte("x\n")}}
	token := "tok"
	_, err := uploadAdaptive(context.Background(), srv.URL, &token, capture.SourceClaudeCode, 0, oneBatch(t, arts), func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want a clear 'too large' error, got %v", err)
	}
}
