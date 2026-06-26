// Package upload is the shared, source-agnostic uploader. It gzips the redacted
// artifact dump and POSTs it to the server's ingest endpoint. Like redact, it
// runs once over all sources and knows nothing about where artifacts came from.
//
// Stub: real upload (gzip tar + device-code auth + POST /ingest) is a follow-up
// ticket. See extension/background.js for the proven wire format to mirror.
package upload

import (
	"context"
	"errors"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
)

// ErrNotImplemented is returned until the uploader is built.
var ErrNotImplemented = errors.New("upload: not implemented yet")

// Upload sends the redacted artifacts to the server.
func Upload(ctx context.Context, arts []capture.Artifact) error {
	// TODO(SK-xxx): gzip dump, device-code auth, POST /ingest per protocol/.
	return ErrNotImplemented
}
