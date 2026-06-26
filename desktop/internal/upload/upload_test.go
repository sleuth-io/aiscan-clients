package upload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestUpload_PostsAndParsesRun(t *testing.T) {
	var gotAuth, gotCT, gotSource, gotWindow, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("authorization")
		gotCT = r.Header.Get("content-type")
		gotSource = r.URL.Query().Get("source")
		gotWindow = r.URL.Query().Get("window_days")
		if r.URL.Path != "/api/aiscan/ingest" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"run":"run-123"}`))
	}))
	defer srv.Close()

	res, err := Upload(context.Background(), Params{
		InstanceURL: srv.URL + "/", // trailing slash should be trimmed
		Token:       "tok",
		Source:      capture.SourceClaudeCode,
		WindowDays:  7,
		Artifacts:   arts(),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotCT != "application/gzip" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotSource != "claude-code" || gotWindow != "7" {
		t.Errorf("query source=%q window_days=%q", gotSource, gotWindow)
	}
	if res.RunID != "run-123" || res.ReportURL != srv.URL+"/aiscan/run-123" || res.Sessions != 2 {
		t.Errorf("result = %#v", res)
	}
}

func TestUpload_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer srv.Close()

	_, err := Upload(context.Background(), Params{InstanceURL: srv.URL, Token: "x", Source: capture.SourceClaudeCode, Artifacts: arts()})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestUpload_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, err := Upload(context.Background(), Params{InstanceURL: srv.URL, Token: "x", Source: capture.SourceClaudeCode, Artifacts: arts()})
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want 500/boom error, got %v", err)
	}
}

func TestUpload_NoArtifacts(t *testing.T) {
	_, err := Upload(context.Background(), Params{InstanceURL: "http://x", Token: "t", Source: capture.SourceClaudeCode})
	if err == nil {
		t.Fatal("want error for empty artifacts")
	}
}
