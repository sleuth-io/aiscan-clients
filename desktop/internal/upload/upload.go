// Package upload is the shared, source-agnostic uploader. It packs the redacted
// artifacts into a gzipped tar and POSTs them to the server's ingest endpoint,
// authorized by a per-user bearer token. Like redact, it runs once over all
// sources and knows nothing about where artifacts came from.
//
// The wire format mirrors the browser extension (extension/background.js): a
// gzipped tar of the redacted dump POSTed to {instance}/api/aiscan/ingest with
// the source and history window as query params, so both clients hit the same
// endpoint identically. The on-disk artifact paths are normalized to the tool's
// native layout (see wireName) so the archive mirrors ~/.claude/projects.
package upload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// ErrUnauthorized is returned when the server rejects the bearer token (401).
// Callers should clear the cached token and re-authorize before retrying.
var ErrUnauthorized = errors.New("upload: unauthorized (token rejected)")

// requestTimeout bounds a single ingest POST so a stalled server can't hang the
// client forever. Generous because the gzipped body can be large.
const requestTimeout = 5 * time.Minute

// maxErrorBody caps how much of an error response we read into a message.
const maxErrorBody = 64 << 10

// Params configures a single upload.
type Params struct {
	InstanceURL string             // e.g. https://app.skills.new (trailing slash optional)
	Token       string             // bearer access token
	Source      capture.SourceID   // wire source label; selects the server parser
	WindowDays  int                // history window reported to the server (0 = all)
	Artifacts   []capture.Artifact // redacted artifacts to upload
}

// Result summarizes a successful upload.
type Result struct {
	ReportURL string // link to the run's report
	RunID     string // server-assigned run id (empty if the server returned none)
	Sessions  int    // number of artifacts uploaded
}

// Upload packs the artifacts into a gzipped tar and POSTs them to
// {instance}/api/aiscan/ingest. It returns ErrUnauthorized on a 401 so the
// caller can refresh the token and retry.
func Upload(ctx context.Context, p Params) (*Result, error) {
	if len(p.Artifacts) == 0 {
		return nil, errors.New("upload: nothing to upload (no artifacts)")
	}
	instance := strings.TrimRight(p.InstanceURL, "/")

	body, err := buildTarGz(p.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("upload: pack: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	endpoint := instance + "/api/aiscan/ingest?source=" +
		url.QueryEscape(string(p.Source)) +
		"&window_days=" + strconv.Itoa(p.WindowDays)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/gzip")
	req.Header.Set("authorization", "Bearer "+p.Token)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	// Bound the read: the body is only used for the run id or an error message,
	// neither of which is large, and we don't want a misbehaving server to
	// stream an unbounded response into memory.
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))

	if res.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("upload: ingest %d: %s", res.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Run string `json:"run"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	reportURL := instance + "/aiscan"
	if parsed.Run != "" {
		reportURL = instance + "/aiscan/" + parsed.Run
	}
	return &Result{ReportURL: reportURL, RunID: parsed.Run, Sessions: len(p.Artifacts)}, nil
}

// buildTarGz writes the artifacts into a gzipped tar of regular-file members.
func buildTarGz(arts []capture.Artifact) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, a := range arts {
		hdr := &tar.Header{
			Name:     wireName(a),
			Mode:     0o644,
			Size:     int64(len(a.Data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(a.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// wireName maps an artifact's logical path to its tar entry name by dropping the
// source-id prefix, so the archive mirrors the source's native on-disk layout —
// e.g. claude-code/projects/p/s.jsonl is stored as projects/p/s.jsonl, mirroring
// ~/.claude/projects, the layout the server's Claude Code parser expects
// (matching the extension's "tar mirroring ~/.claude/projects"). Tying the strip
// to the actual source id leaves any path without that prefix untouched.
func wireName(a capture.Artifact) string {
	return strings.TrimPrefix(a.Path, string(a.Source)+"/")
}
