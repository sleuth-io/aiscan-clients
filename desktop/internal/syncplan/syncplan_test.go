package syncplan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlan_PostsAvailableAndParsesNeeded(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	var gotPath, gotAuth, gotCT string
	var gotReq struct {
		Variables struct {
			Source        string `json:"source"`
			SchemaVersion int    `json:"schemaVersion"`
			Available     []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"available"`
		} `json:"variables"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		gotCT = r.Header.Get("content-type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.Write([]byte(`{"data":{"aiscanSyncPlan":{"neededSpans":[` +
			`{"start":"2026-06-10T00:00:00Z","end":"2026-06-29T00:00:00Z"}]}}}`))
	}))
	defer srv.Close()

	needed, err := Plan(context.Background(), srv.URL, "tok", "claude-code", 1, []Span{{Start: start, End: end}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if gotPath != "/graphql" || gotAuth != "Bearer tok" || gotCT != "application/json" {
		t.Errorf("request path=%q auth=%q ct=%q", gotPath, gotAuth, gotCT)
	}
	if gotReq.Variables.Source != "claude-code" || gotReq.Variables.SchemaVersion != 1 {
		t.Errorf("variables = %#v", gotReq.Variables)
	}
	if len(gotReq.Variables.Available) != 1 ||
		gotReq.Variables.Available[0].Start != "2026-06-01T00:00:00Z" ||
		gotReq.Variables.Available[0].End != "2026-06-29T00:00:00Z" {
		t.Errorf("available = %#v", gotReq.Variables.Available)
	}
	want := []Span{{
		Start: time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
	}}
	if len(needed) != len(want) || !needed[0].Start.Equal(want[0].Start) || !needed[0].End.Equal(want[0].End) {
		t.Errorf("needed = %#v, want %#v", needed, want)
	}
}

func TestPlan_EmptyNeededSpans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"aiscanSyncPlan":{"neededSpans":[]}}}`))
	}))
	defer srv.Close()

	needed, err := Plan(context.Background(), srv.URL, "tok", "claude-code", 1, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(needed) != 0 {
		t.Errorf("want no needed spans, got %#v", needed)
	}
}

func TestPlan_GraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"aiscan not enabled"}]}`))
	}))
	defer srv.Close()

	_, err := Plan(context.Background(), srv.URL, "tok", "claude-code", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "aiscan not enabled") {
		t.Fatalf("want graphql error, got %v", err)
	}
}

func TestPlan_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Plan(context.Background(), srv.URL, "tok", "claude-code", 1, nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}
