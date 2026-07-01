// Package upload is the shared, source-agnostic uploader. It packs the redacted
// artifacts into a gzipped tar and POSTs them to the server's ingest endpoint as
// evidence for a declared capture span, authorized by a per-user bearer token.
// Like redact, it runs once over all sources and knows nothing about where
// artifacts came from.
//
// The wire format mirrors the browser extension (extension/background.js): a
// gzipped tar of the redacted dump POSTed to {instance}/api/aiscan/ingest with
// the source, capture span, and schema version as query params. The on-disk
// artifact paths are normalized to the tool's native layout (see wireName) so
// the archive mirrors ~/.claude/projects.
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
// carry. The server streams the request body now (it no longer reads it through
// request.body), so the gate is the app's own MAX_UPLOAD_BYTES (50 MiB) rather
// than Django's much smaller DATA_UPLOAD_MAX_MEMORY_SIZE. We stay a few MiB under
// it so a typical history uploads as a single batch — one scan session — and
// only a very large one is split (see SplitForUpload). The CLI's adaptive 413
// fallback still covers a server or proxy that rejects a body below this.
const MaxCompressedBytes = 45 << 20

// SchemaVersionV1 is the capture schema this client collects under. It records
// what the client captured (not how the server derives from it) and is sent with
// every evidence upload and sync-plan query so the server can scope coverage.
const SchemaVersionV1 = 1

// EvidenceParams configures a single evidence upload to the v1 sync endpoint.
type EvidenceParams struct {
	InstanceURL   string           // e.g. https://app.skills.new (trailing slash optional)
	Token         string           // bearer access token
	Source        capture.SourceID // wire source label; selects the server parser
	CapturedStart time.Time        // declared window lower bound (inclusive)
	CapturedEnd   time.Time        // declared window upper bound
	SchemaVersion int              // capture schema version (SchemaVersionV1)
	Sessions      int              // artifact count, carried through to the result
}

// EvidenceResult summarizes a successful evidence upload.
type EvidenceResult struct {
	EvidenceGID string // server-assigned evidence gid
	Sessions    int    // number of artifacts uploaded (0 for an empty window)
}

// UploadEvidence POSTs a gzipped tar body (or an empty body, for a confirmed-
// empty window) to {instance}/api/aiscan/ingest with the declared span and
// schema version as query params. It targets the v1 sync contract: the span —
// not a history-window count — is the metadata, and a
// zero-length body is valid (it records that the window was scanned and found
// empty). It returns ErrUnauthorized on 401 and ErrPayloadTooLarge on 413 so the
// caller can re-authorize or split and retry.
func UploadEvidence(ctx context.Context, p EvidenceParams, body []byte) (*EvidenceResult, error) {
	instance := strings.TrimRight(p.InstanceURL, "/")

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	q := url.Values{}
	q.Set("source", string(p.Source))
	q.Set("captured_start", p.CapturedStart.UTC().Format(time.RFC3339))
	q.Set("captured_end", p.CapturedEnd.UTC().Format(time.RFC3339))
	q.Set("schema_version", strconv.Itoa(p.SchemaVersion))
	endpoint := instance + "/api/aiscan/ingest?" + q.Encode()

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
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))

	if res.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if res.StatusCode == http.StatusRequestEntityTooLarge {
		return nil, ErrPayloadTooLarge
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("upload: evidence %d: %s", res.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Evidence string `json:"evidence"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	return &EvidenceResult{EvidenceGID: parsed.Evidence, Sessions: p.Sessions}, nil
}

// Batch is a group of artifacts and the gzipped tar body that carries them,
// sized to fit a single ingest POST. SplitForUpload builds Body while measuring,
// so the eventual UploadEvidence reuses it instead of gzipping again.
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
