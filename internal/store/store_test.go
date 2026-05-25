package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

func TestListSessionsFiltersFinished(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	seed := []struct {
		id     string
		status session.State
	}{
		{"run", session.StateRunning},
		{"wait", session.StateWaiting},
		{"done", session.StateFinished},
		{"boom", session.StateFailed},
	}
	for _, s := range seed {
		if err := st.CreateSession(ctx, session.Session{
			ID: s.id, Agent: "claude", CWD: "/tmp", HostID: "h",
			StartedAt: now, LastEventAt: now, Status: s.status,
		}); err != nil {
			t.Fatal(err)
		}
	}

	active, err := st.ListSessions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 3 {
		t.Fatalf("default list: want 3 sessions (finished hidden), got %d", len(active))
	}
	for _, s := range active {
		if s.Status == session.StateFinished {
			t.Errorf("default list included finished session %q", s.ID)
		}
	}

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("--all list: want 4 sessions, got %d", len(all))
	}
}
