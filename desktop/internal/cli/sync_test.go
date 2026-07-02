package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
	"github.com/sleuth-io/aiscan-clients/desktop/internal/syncplan"
)

// ingestServer is a fake ingest endpoint that accepts (202) any gzip body at or
// below limit and rejects (413) anything larger, tallying the session names it
// actually received so a test can assert what made it onto the wire. A limit of
// 0 means accept everything.
type ingestServer struct {
	*httptest.Server
	mu       sync.Mutex
	limit    int
	posts    int
	rejected int
	got      []string // tar entry names across all accepted bodies
}

func newIngestServer(limit int) *ingestServer {
	s := &ingestServer{limit: limit}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.posts++
		if s.limit > 0 && len(body) > s.limit {
			s.rejected++
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			w.Write([]byte("<html>413</html>"))
			return
		}
		s.got = append(s.got, tarNames(body)...)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"evidence":"EV-1"}`))
	}))
	return s
}

// tarNames lists the entry names in a gzipped tar body (empty for a zero-length
// confirmed-empty body).
func tarNames(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	return names
}

// incompressibleArt returns an artifact of n random (so gzip-resistant) bytes at
// the given path, sized to land above or below a batch cap on purpose.
func incompressibleArt(t *testing.T, path string, n int) capture.Artifact {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return capture.Artifact{Source: capture.SourceClaudeCode, Path: path, Data: b}
}

func testSpan() syncplan.Span {
	return syncplan.Span{
		Start: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	}
}

// A single session whose own compressed body exceeds the cap is skipped up front
// — never POSTed — while its normal-sized siblings still upload.
func TestUploadArtifacts_SkipsOversizedUpFront(t *testing.T) {
	srv := newIngestServer(0) // server accepts anything; the cap is client-side
	defer srv.Close()

	const cap = 8 << 10
	arts := []capture.Artifact{
		incompressibleArt(t, "claude-code/projects/a.jsonl", 1<<10),
		incompressibleArt(t, "claude-code/projects/huge.jsonl", 64<<10), // well over cap
		incompressibleArt(t, "claude-code/projects/b.jsonl", 1<<10),
	}

	tok := "tok"
	var out bytes.Buffer
	uploaded, err := uploadArtifacts(context.Background(), syncConfig{instance: srv.URL},
		&tok, capture.Recipe{ID: capture.SourceClaudeCode}, testSpan(), arts, cap, nil, &out)
	if err != nil {
		t.Fatalf("uploadArtifacts: %v", err)
	}
	if uploaded != 2 {
		t.Errorf("uploaded = %d, want 2 (the huge one skipped)", uploaded)
	}
	for _, n := range srv.got {
		if strings.Contains(n, "huge") {
			t.Errorf("oversized session reached the wire: %q", n)
		}
	}
	if !strings.Contains(out.String(), "skipped") || !strings.Contains(out.String(), "huge.jsonl") {
		t.Errorf("expected a skip warning naming huge.jsonl, got:\n%s", out.String())
	}
}

// When the server's real limit is below the client's cap, a batch the client
// thought fine gets a 413; the client re-splits smaller and retries until every
// session lands.
func TestUploadArtifacts_ReSplitsOn413(t *testing.T) {
	const serverLimit = 6 << 10
	srv := newIngestServer(serverLimit)
	defer srv.Close()

	// Six ~2 KiB sessions: individually fine, but a single batch of all six
	// exceeds serverLimit, so the first POST 413s and forces re-splitting.
	var arts []capture.Artifact
	for i := 0; i < 6; i++ {
		arts = append(arts, incompressibleArt(t, fmt.Sprintf("claude-code/projects/s%d.jsonl", i), 2<<10))
	}

	tok := "tok"
	var out bytes.Buffer
	uploaded, err := uploadArtifacts(context.Background(), syncConfig{instance: srv.URL},
		&tok, capture.Recipe{ID: capture.SourceClaudeCode}, testSpan(), arts, 1<<20 /* generous client cap */, nil, &out)
	if err != nil {
		t.Fatalf("uploadArtifacts: %v", err)
	}
	if uploaded != 6 {
		t.Errorf("uploaded = %d, want 6 (all recovered via re-split)", uploaded)
	}
	if len(srv.got) != 6 {
		t.Errorf("server ingested %d sessions, want 6", len(srv.got))
	}
	if srv.rejected == 0 {
		t.Error("expected at least one 413 to exercise the re-split backstop")
	}
}

// A lone session the server rejects (fits the client cap but exceeds the
// server's) is skipped rather than failing the whole sync.
func TestUploadArtifacts_SkipsLoneSessionRejectedByServer(t *testing.T) {
	const serverLimit = 1 << 10
	srv := newIngestServer(serverLimit)
	defer srv.Close()

	arts := []capture.Artifact{
		incompressibleArt(t, "claude-code/projects/lone.jsonl", 8<<10), // over serverLimit, under client cap
	}

	tok := "tok"
	var out bytes.Buffer
	uploaded, err := uploadArtifacts(context.Background(), syncConfig{instance: srv.URL},
		&tok, capture.Recipe{ID: capture.SourceClaudeCode}, testSpan(), arts, 1<<20, nil, &out)
	if err != nil {
		t.Fatalf("uploadArtifacts: %v", err)
	}
	if uploaded != 0 {
		t.Errorf("uploaded = %d, want 0 (lone session skipped)", uploaded)
	}
	if !strings.Contains(out.String(), "skipped") || !strings.Contains(out.String(), "lone.jsonl") {
		t.Errorf("expected a skip warning naming lone.jsonl, got:\n%s", out.String())
	}
}
