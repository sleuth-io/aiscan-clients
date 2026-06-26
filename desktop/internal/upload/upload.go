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

// ErrPayloadTooLarge is returned when the server rejects the body as too large
// (413). Callers should split the batch into smaller uploads and retry — the
// server's exact size limit is not known to the client.
var ErrPayloadTooLarge = errors.New("upload: payload too large (413)")

// requestTimeout bounds a single ingest POST so a stalled server can't hang the
// client forever. Generous because the gzipped body can be large.
const requestTimeout = 5 * time.Minute

// maxErrorBody caps how much of an error response we read into a message.
const maxErrorBody = 64 << 10

// MaxCompressedBytes is the largest gzipped body a single ingest POST should
// carry. The server reads the whole request body into memory, so Django's
// DATA_UPLOAD_MAX_MEMORY_SIZE (20 MiB) — not the app's 50 MiB MAX_UPLOAD_BYTES —
// is the real gate: a larger body is rejected with a 413 before the view runs.
// We stay a few MiB under it; a heavy local history is split into several
// uploads (see SplitForUpload).
const MaxCompressedBytes = 18 << 20

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
	body, err := buildTarGz(p.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("upload: pack: %w", err)
	}
	return UploadPacked(ctx, p, body)
}

// UploadPacked POSTs an already-gzipped body (e.g. the one SplitForUpload built
// to size the batch), so a caller that packed the artifacts to measure them does
// not pay to gzip them again. p.Artifacts is used only for the session count.
func UploadPacked(ctx context.Context, p Params, body []byte) (*Result, error) {
	if len(p.Artifacts) == 0 {
		return nil, errors.New("upload: nothing to upload (no artifacts)")
	}
	instance := strings.TrimRight(p.InstanceURL, "/")

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
	if res.StatusCode == http.StatusRequestEntityTooLarge {
		return nil, ErrPayloadTooLarge
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

// Batch is a group of artifacts and the gzipped tar body that carries them,
// sized to fit a single ingest POST. SplitForUpload builds Body while measuring,
// so the eventual UploadPacked reuses it instead of gzipping again.
type Batch struct {
	Artifacts []capture.Artifact
	Body      []byte
}

// SplitForUpload groups arts into batches whose gzipped tar body each stays at
// or below maxCompressed bytes, so every batch fits in one ingest POST. It
// measures the actual compressed size (rather than guessing a ratio) and halves
// a too-big group, so batches are as large as the limit allows — the fewest
// uploads, hence the fewest separate reports. A single artifact whose own
// compressed body still exceeds the limit is returned alone; the caller decides
// what to do if the server then rejects it.
func SplitForUpload(arts []capture.Artifact, maxCompressed int) ([]Batch, error) {
	if len(arts) == 0 {
		return nil, nil
	}
	body, err := buildTarGz(arts)
	if err != nil {
		return nil, err
	}
	if len(body) <= maxCompressed || len(arts) == 1 {
		return []Batch{{Artifacts: arts, Body: body}}, nil
	}
	mid := len(arts) / 2
	left, err := SplitForUpload(arts[:mid], maxCompressed)
	if err != nil {
		return nil, err
	}
	right, err := SplitForUpload(arts[mid:], maxCompressed)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
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
