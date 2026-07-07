package tray

import (
	"strings"
	"testing"
	"time"
)

func TestStatusLineCapsLongError(t *testing.T) {
	st := State{Username: "u", LastErr: strings.Repeat("x", 500)}
	line := statusLine(st)
	// "Problem: " prefix + at most maxStatusLen runes + a one-rune ellipsis.
	if got := len([]rune(line)); got > len("Problem: ")+maxStatusLen+1 {
		t.Fatalf("status line too long: %d runes", got)
	}
	if !strings.HasSuffix(line, "…") {
		t.Fatalf("truncated line should end with an ellipsis, got %q", line)
	}
}

func TestStatusLineCollapsesMultilineError(t *testing.T) {
	st := State{Username: "u", LastErr: "upload failed:\n  connection refused\n  retrying"}
	line := statusLine(st)
	if strings.ContainsAny(line, "\n\r\t") {
		t.Fatalf("status line should be a single line, got %q", line)
	}
	if strings.Contains(line, "  ") {
		t.Fatalf("status line should collapse whitespace runs, got %q", line)
	}
}

func TestStatusLineShortErrorUntouched(t *testing.T) {
	st := State{Username: "u", LastErr: "connection refused"}
	if got, want := statusLine(st), "Problem: connection refused"; got != want {
		t.Fatalf("statusLine = %q, want %q", got, want)
	}
}

func TestStatusLinePriority(t *testing.T) {
	// Syncing outranks a pending error.
	st := State{Username: "u", Syncing: true, LastErr: "boom"}
	if got := statusLine(st); got != "Syncing…" {
		t.Fatalf("statusLine = %q, want Syncing…", got)
	}
	// Logged out with no trouble hides the line.
	if got := statusLine(State{LastSync: time.Time{}}); got != "" {
		t.Fatalf("statusLine = %q, want empty", got)
	}
}
