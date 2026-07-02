package upload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

func arts() []capture.Artifact {
	return []capture.Artifact{
		{Source: capture.SourceClaudeCode, Path: "claude-code/projects/p/s1.jsonl", Data: []byte("a\n")},
		{Source: capture.SourceClaudeCode, Path: "claude-code/projects/p/s2.jsonl", Data: []byte("bb\n")},
	}
}

// readTarGz returns name->content for every member of a gzipped tar.
func readTarGz(t *testing.T, body []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
	return out
}

func TestBuildTarGz_NormalizesNamesAndContent(t *testing.T) {
	body, err := buildTarGz(arts())
	if err != nil {
		t.Fatalf("buildTarGz: %v", err)
	}
	got := readTarGz(t, body)
	// The leading source-id segment (claude-code/) is stripped so the archive
	// mirrors ~/.claude/projects.
	if got["projects/p/s1.jsonl"] != "a\n" || got["projects/p/s2.jsonl"] != "bb\n" {
		t.Fatalf("unexpected tar members: %#v", got)
	}
	if _, ok := got["claude-code/projects/p/s1.jsonl"]; ok {
		t.Fatalf("source-id prefix was not stripped: %#v", got)
	}
}

// randArts returns n artifacts each carrying size bytes of incompressible data,
// so gzip can't shrink them and compressed size tracks the byte count — letting
// the test force splits deterministically.
func randArts(t *testing.T, n, size int) []capture.Artifact {
	t.Helper()
	r := rand.New(rand.NewSource(1)) // fixed seed: deterministic
	out := make([]capture.Artifact, n)
	for i := range out {
		b := make([]byte, size)
		r.Read(b)
		out[i] = capture.Artifact{Source: capture.SourceClaudeCode, Path: "claude-code/projects/p/s.jsonl", Data: b}
	}
	return out
}

func TestSplitForUpload_KeepsBatchesUnderLimit(t *testing.T) {
	in := randArts(t, 10, 4096) // ~40 KiB incompressible total
	const max = 12 << 10        // 12 KiB ceiling → expect ~4 batches

	batches, oversized, err := SplitForUpload(in, max)
	if err != nil {
		t.Fatalf("SplitForUpload: %v", err)
	}
	if len(oversized) != 0 {
		t.Fatalf("no single artifact exceeds the ceiling, want none oversized, got %d", len(oversized))
	}
	if len(batches) < 2 {
		t.Fatalf("expected the oversized set to be split, got %d batch(es)", len(batches))
	}
	total := 0
	for _, b := range batches {
		// The Body is the measured wire body; multi-artifact batches must fit
		// (a lone artifact may legitimately exceed the ceiling).
		if len(b.Artifacts) > 1 && len(b.Body) > max {
			t.Errorf("batch of %d compresses to %d > %d", len(b.Artifacts), len(b.Body), max)
		}
		if len(b.Body) == 0 {
			t.Error("batch carries no prebuilt body")
		}
		total += len(b.Artifacts)
	}
	if total != len(in) {
		t.Errorf("batches cover %d artifacts, want %d", total, len(in))
	}
}

func TestSplitForUpload_LoneOversizedArtifact(t *testing.T) {
	in := randArts(t, 1, 8192)
	batches, oversized, err := SplitForUpload(in, 1024) // far below the single artifact's size
	if err != nil {
		t.Fatalf("SplitForUpload: %v", err)
	}
	if len(batches) != 0 {
		t.Fatalf("a lone oversized artifact should not be batched, got %d batch(es)", len(batches))
	}
	if len(oversized) != 1 {
		t.Fatalf("a lone oversized artifact should be reported oversized, got %d", len(oversized))
	}
}

func TestUploadEvidence_PostsSpanAndParsesEvidence(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	var gotPath, gotSource, gotStart, gotEnd, gotSchema, gotCT, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSource = r.URL.Query().Get("source")
		gotStart = r.URL.Query().Get("captured_start")
		gotEnd = r.URL.Query().Get("captured_end")
		gotSchema = r.URL.Query().Get("schema_version")
		gotCT = r.Header.Get("content-type")
		gotAuth = r.Header.Get("authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"evidence":"EV-abc"}`))
	}))
	defer srv.Close()

	body, _ := buildTarGz(arts())
	res, err := UploadEvidence(context.Background(), EvidenceParams{
		InstanceURL:   srv.URL + "/",
		Token:         "tok",
		Source:        capture.SourceClaudeCode,
		CapturedStart: start,
		CapturedEnd:   end,
		SchemaVersion: SchemaVersionV1,
		Sessions:      2,
	}, body)
	if err != nil {
		t.Fatalf("UploadEvidence: %v", err)
	}
	if gotPath != "/api/aiscan/ingest" {
		t.Errorf("path = %q", gotPath)
	}
	if gotSource != "claude-code" || gotStart != "2026-06-01T00:00:00Z" || gotEnd != "2026-06-29T00:00:00Z" || gotSchema != "1" {
		t.Errorf("query source=%q start=%q end=%q schema=%q", gotSource, gotStart, gotEnd, gotSchema)
	}
	if gotCT != "application/gzip" || gotAuth != "Bearer tok" {
		t.Errorf("headers ct=%q auth=%q", gotCT, gotAuth)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(gotBody), len(body))
	}
	if res.EvidenceGID != "EV-abc" || res.Sessions != 2 {
		t.Errorf("result = %#v", res)
	}
}

func TestUploadEvidence_EmptyWindowPostsEmptyBody(t *testing.T) {
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"evidence":"EV-empty"}`))
	}))
	defer srv.Close()

	res, err := UploadEvidence(context.Background(), EvidenceParams{
		InstanceURL:   srv.URL,
		Token:         "tok",
		Source:        capture.SourceClaudeCode,
		CapturedStart: time.Now().UTC().Add(-time.Hour),
		CapturedEnd:   time.Now().UTC(),
		SchemaVersion: SchemaVersionV1,
		Sessions:      0,
	}, nil)
	if err != nil {
		t.Fatalf("UploadEvidence: %v", err)
	}
	if gotLen != 0 {
		t.Errorf("empty window should send a zero-length body, got %d", gotLen)
	}
	if res.EvidenceGID != "EV-empty" || res.Sessions != 0 {
		t.Errorf("result = %#v", res)
	}
}

func TestUploadEvidence_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := UploadEvidence(context.Background(), EvidenceParams{
		InstanceURL: srv.URL, Token: "x", Source: capture.SourceClaudeCode,
		CapturedStart: time.Now(), CapturedEnd: time.Now(), SchemaVersion: SchemaVersionV1,
	}, nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestUploadEvidence_PayloadTooLarge(t *testing.T) {
	defer fastBackoff(t)()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.Write([]byte("<html>413</html>"))
	}))
	defer srv.Close()

	body, _ := buildTarGz(arts())
	_, err := UploadEvidence(context.Background(), EvidenceParams{
		InstanceURL: srv.URL, Token: "x", Source: capture.SourceClaudeCode,
		CapturedStart: time.Now(), CapturedEnd: time.Now(), SchemaVersion: SchemaVersionV1,
	}, body)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("want ErrPayloadTooLarge, got %v", err)
	}
	// 413 is permanent — it must be surfaced immediately, never retried.
	if got := calls.Load(); got != 1 {
		t.Errorf("413 was POSTed %d times; it must not be retried", got)
	}
}

// fastBackoff shrinks the retry backoff so retry tests run in milliseconds, and
// returns a restore func for defer.
func fastBackoff(t *testing.T) func() {
	t.Helper()
	base, max := transientBackoffBase, transientBackoffMax
	transientBackoffBase, transientBackoffMax = time.Millisecond, 2*time.Millisecond
	return func() { transientBackoffBase, transientBackoffMax = base, max }
}

// A gateway hiccup (502/503/504) or network blip is transient: retry with
// backoff until it clears. Ingest is idempotent, so the retries don't duplicate.
func TestUploadEvidence_RetriesTransient5xx(t *testing.T) {
	defer fastBackoff(t)()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway) // 502 twice, then recover
			return
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"evidence":"EV-ok"}`))
	}))
	defer srv.Close()

	body, _ := buildTarGz(arts())
	res, err := UploadEvidence(context.Background(), EvidenceParams{
		InstanceURL: srv.URL, Token: "x", Source: capture.SourceClaudeCode,
		CapturedStart: time.Now(), CapturedEnd: time.Now(), SchemaVersion: SchemaVersionV1,
	}, body)
	if err != nil {
		t.Fatalf("want success after retries, got %v", err)
	}
	if res.EvidenceGID != "EV-ok" {
		t.Errorf("evidence = %q, want EV-ok", res.EvidenceGID)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server saw %d posts, want 3 (2×502 + 1×202)", got)
	}
}

// A server that never recovers exhausts the retries and returns an error —
// after exactly the bounded number of attempts, not forever.
func TestUploadEvidence_GivesUpOnPersistent5xx(t *testing.T) {
	defer fastBackoff(t)()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // 503 forever
	}))
	defer srv.Close()

	body, _ := buildTarGz(arts())
	_, err := UploadEvidence(context.Background(), EvidenceParams{
		InstanceURL: srv.URL, Token: "x", Source: capture.SourceClaudeCode,
		CapturedStart: time.Now(), CapturedEnd: time.Now(), SchemaVersion: SchemaVersionV1,
	}, body)
	if err == nil {
		t.Fatal("want an error after exhausting retries, got nil")
	}
	if got := calls.Load(); got != transientMaxRetries+1 {
		t.Errorf("server saw %d posts, want %d (1 initial + %d retries)", got, transientMaxRetries+1, transientMaxRetries)
	}
}
