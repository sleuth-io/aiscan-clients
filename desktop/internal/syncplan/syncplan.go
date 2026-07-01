// Package syncplan is the GraphQL half of the v1 sync contract: it asks the
// server which spans of a source's history are still missing. The client reports
// the raw available span (its earliest data through now) and the server returns
// the exact work-list — available minus coverage, with the look-back floor and
// schema scope already applied — so the client does no interval math of its own.
//
// The request is hand-rolled JSON over net/http (matching upload.go's style)
// rather than a GraphQL client library: one query, a fixed shape, no codegen.
package syncplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnauthorized is returned when the server rejects the bearer token (401), so
// the caller can clear the cached token and re-authorize before retrying — the
// same dance the upload package uses.
var ErrUnauthorized = errors.New("syncplan: unauthorized (token rejected)")

// requestTimeout bounds a single plan query. The payload is small JSON, so this
// is short relative to an evidence upload.
const requestTimeout = 30 * time.Second

// maxResponseBody caps how much of the response we read into memory.
const maxResponseBody = 1 << 20

// planQuery is the fixed aiscanSyncPlan query. available is the discovered span
// set; neededSpans is exactly what to capture and upload, per source.
const planQuery = `query Plan($source: String!, $schemaVersion: Int!, $available: [AiScanSpanInput!]!) {
  aiscanSyncPlan(source: $source, schemaVersion: $schemaVersion, available: $available) {
    neededSpans { start end }
  }
}`

// Span is a half-open time window [Start, End). It is both the unit the client
// reports as available and the unit the server returns as needed.
type Span struct {
	Start time.Time
	End   time.Time
}

// Plan calls aiscanSyncPlan at {instance}/graphql and returns the spans the
// server still needs for the source. An empty result means nothing to upload.
func Plan(ctx context.Context, instanceURL, token, source string, schemaVersion int, available []Span) ([]Span, error) {
	instance := strings.TrimRight(instanceURL, "/")

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	avail := make([]map[string]string, 0, len(available))
	for _, s := range available {
		avail = append(avail, map[string]string{
			"start": s.Start.UTC().Format(time.RFC3339),
			"end":   s.End.UTC().Format(time.RFC3339),
		})
	}
	payload := map[string]any{
		"query": planQuery,
		"variables": map[string]any{
			"source":        source,
			"schemaVersion": schemaVersion,
			"available":     avail,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, instance+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+token)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, maxResponseBody))

	if res.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("syncplan: plan %d: %s", res.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Data struct {
			AiscanSyncPlan struct {
				NeededSpans []struct {
					Start string `json:"start"`
					End   string `json:"end"`
				} `json:"neededSpans"`
			} `json:"aiscanSyncPlan"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("syncplan: decode response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		msgs := make([]string, 0, len(parsed.Errors))
		for _, e := range parsed.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("syncplan: graphql error: %s", strings.Join(msgs, "; "))
	}

	out := make([]Span, 0, len(parsed.Data.AiscanSyncPlan.NeededSpans))
	for _, s := range parsed.Data.AiscanSyncPlan.NeededSpans {
		start, err := time.Parse(time.RFC3339, s.Start)
		if err != nil {
			return nil, fmt.Errorf("syncplan: parse span start %q: %w", s.Start, err)
		}
		end, err := time.Parse(time.RFC3339, s.End)
		if err != nil {
			return nil, fmt.Errorf("syncplan: parse span end %q: %w", s.End, err)
		}
		out = append(out, Span{Start: start, End: end})
	}
	return out, nil
}
