package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/alert"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
	"github.com/just-barcodes/agentic-sessions-manager/internal/store"
)

// TestSweepReapsAndRefreshesCount verifies the sweep both marks a dead session
// and fires the sinks, so the waiting-count file (read by status bars) no longer
// counts a session whose agent has gone.
func TestSweepReapsAndRefreshesCount(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	const host = "h"

	// A waiting session whose process is provably gone: a boot id that cannot
	// match the current boot makes liveness.Alive return false deterministically.
	if err := st.CreateSession(ctx, session.Session{
		ID: "dead", Agent: "claude", CWD: "/tmp", HostID: host,
		StartedAt: now, LastEventAt: now, Status: session.StateWaiting,
		PID: 4242, PIDStart: 1, BootID: "not-the-current-boot",
	}); err != nil {
		t.Fatal(err)
	}

	countPath := filepath.Join(dir, "waiting-count")
	if err := os.WriteFile(countPath, []byte("1\n"), 0o644); err != nil { // stale count
		t.Fatal(err)
	}
	h := &handler{
		store:  st,
		hostID: host,
		sinks: []alert.Sink{alert.CountFile{
			Path:  countPath,
			Count: func() (int, error) { return st.CountByStatus(ctx, session.StateWaiting) },
		}},
	}

	h.sweep(ctx)

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != session.StateDead {
		t.Fatalf("sweep did not mark the session dead: %+v", all)
	}

	b, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("count file not written: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "0" {
		t.Errorf("waiting-count = %q after sweep, want 0", got)
	}
}
