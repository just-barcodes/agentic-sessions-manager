package store

import (
	"context"
	"database/sql"
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

func TestReapStale(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	mk := func(id string, status session.State, pid int) {
		if err := st.CreateSession(ctx, session.Session{
			ID: id, Agent: "claude", CWD: "/tmp", HostID: "h",
			StartedAt: now, LastEventAt: now, Status: status, PID: pid, BootID: "b",
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("alive", session.StateRunning, 1)
	mk("dead", session.StateWaiting, 2)
	mk("noident", session.StateRunning, 0) // un-probeable: must be skipped
	mk("done", session.StateFinished, 3)   // terminal: must be skipped

	// Only pid 2 is reported dead; the callback must never see noident/done.
	reaped, err := st.ReapStale(ctx, "h", func(s session.Session) bool {
		if s.PID == 0 || s.Status == session.StateFinished {
			t.Errorf("callback received un-probeable/terminal session %q", s.ID)
		}
		return s.PID == 2
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].ID != "dead" {
		t.Fatalf("want only [dead] reaped, got %+v", reaped)
	}
	if reaped[0].Status != session.StateDead {
		t.Errorf("reaped session status = %q, want %q", reaped[0].Status, session.StateDead)
	}

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]session.State{}
	for _, s := range all {
		got[s.ID] = s.Status
	}
	if got["dead"] != session.StateDead {
		t.Errorf("dead session: want %q, got %q", session.StateDead, got["dead"])
	}
	if got["alive"] != session.StateRunning {
		t.Errorf("alive session wrongly changed to %q", got["alive"])
	}
	if got["noident"] != session.StateRunning {
		t.Errorf("un-probeable session was reaped to %q", got["noident"])
	}

	// A reaped session drops out of the default (active) list.
	active, err := st.ListSessions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range active {
		if s.ID == "dead" {
			t.Error("default list still shows the reaped session")
		}
	}
}

// TestMigrateExistingDB guards real user data: opening a database created before
// the identity columns existed must migrate in place without dropping rows.
func TestMigrateExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sm.db")

	// Stand up the pre-identity sessions table and a row, the way an older sm
	// would have left it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, agent TEXT NOT NULL, native_id TEXT NOT NULL DEFAULT '',
		cwd TEXT NOT NULL, host_id TEXT NOT NULL, started_at INTEGER NOT NULL,
		last_event_at INTEGER NOT NULL, status TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO sessions VALUES('old','claude','','/tmp','h',0,0,'running')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-identity DB: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "old" {
		t.Fatalf("existing row not preserved across migration: %+v", all)
	}

	// The migrated row has pid 0, so it is un-probeable and must never be reaped.
	reaped, err := st.ReapStale(ctx, "h", func(session.Session) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 0 {
		t.Errorf("pre-identity row (pid 0) is un-probeable, but %d reaped", len(reaped))
	}
}
