package alert

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

// TestCountFileWritesCount verifies OnStateChange writes the current count
// (creating the directory) regardless of the session it is handed — the sink
// derives its value from Count, not the event.
func TestCountFileWritesCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "waiting-count")
	c := CountFile{Path: path, Count: func() (int, error) { return 3, nil }}

	if err := c.OnStateChange("s1", session.StateWaiting); err != nil {
		t.Fatalf("OnStateChange: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("count file not written: %v", err)
	}
	if string(b) != "3\n" {
		t.Errorf("count file = %q, want %q", string(b), "3\n")
	}
}

// TestCountFileCountError verifies a failing Count propagates and no file is
// written.
func TestCountFileCountError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waiting-count")
	sentinel := errors.New("count failed")
	c := CountFile{Path: path, Count: func() (int, error) { return 0, sentinel }}

	if err := c.OnStateChange("s1", session.StateWaiting); !errors.Is(err, sentinel) {
		t.Fatalf("OnStateChange error = %v, want %v", err, sentinel)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("count file should not exist after a Count error, stat err = %v", err)
	}
}
