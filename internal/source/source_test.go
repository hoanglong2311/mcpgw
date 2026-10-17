package source

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEventKindString(t *testing.T) {
	tests := map[EventKind]string{
		ToolsChanged:     "ToolsChanged",
		PromptsChanged:   "PromptsChanged",
		ResourcesChanged: "ResourcesChanged",
		ResourceUpdated:  "ResourceUpdated",
		Progress:         "Progress",
		Connected:        "Connected",
		Stopped:          "Stopped",
		EventKind(42):    "EventKind(42)",
	}
	for k, want := range tests {
		if got := k.String(); got != want {
			t.Errorf("%d: got %q want %q", int(k), got, want)
		}
	}
}

func TestBackoffError(t *testing.T) {
	last := errors.New("boom")
	err := error(&BackoffError{Last: last, RetryAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	if !errors.Is(err, last) {
		t.Error("Unwrap should expose Last")
	}
	var be *BackoffError
	if !errors.As(err, &be) {
		t.Fatal("errors.As failed")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "2026-01-02T03:04:05Z") {
		t.Errorf("message %q", err.Error())
	}
}
