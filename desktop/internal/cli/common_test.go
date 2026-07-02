package cli

import (
	"testing"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/capture"
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
