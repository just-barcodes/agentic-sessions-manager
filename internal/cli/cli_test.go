package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
	"github.com/just-barcodes/agentic-sessions-manager/internal/store"
)

// TestFocusRemoteSession verifies focus refuses a session living on another
// host with a message naming that host, instead of probing a meaningless pid
// against the local /proc.
func TestFocusRemoteSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // openStore resolves through DataDir
	if err := os.MkdirAll(filepath.Dir(store.DefaultDBPath()), 0o700); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(store.DefaultDBPath())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.CreateSession(context.Background(), session.Session{
		ID: "remote1", Agent: "claude", CWD: "/tmp", HostID: "another-host",
		StartedAt: now, LastEventAt: now, Status: session.StateWaiting, PID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	err = Focus([]string{"remote1"})
	if err == nil {
		t.Fatal("Focus on a remote session succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "another-host") {
		t.Errorf("error %q does not name the remote host", err)
	}
}
