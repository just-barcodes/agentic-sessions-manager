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
		{"between", session.StateIdle}, // alive, between turns: must stay visible
		{"done", session.StateFinished},
		{"gone", session.StateDead}, // reaped: terminal, must be hidden by default
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
	if len(active) != 4 {
		t.Fatalf("default list: want 4 sessions (finished + dead hidden), got %d", len(active))
	}
	for _, s := range active {
		if session.IsTerminal(s.Status) {
			t.Errorf("default list included terminal session %q (%s)", s.ID, s.Status)
		}
	}

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("--all list: want 6 sessions, got %d", len(all))
	}
}

// TestUpdateStatusRecencyGuard verifies a stale event (older timestamp) cannot
// overwrite state set by a newer one, while an equal-or-newer event still applies.
func TestUpdateStatusRecencyGuard(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	t0 := time.Unix(1_000_000, 0)
	if err := st.CreateSession(ctx, session.Session{
		ID: "s", Agent: "claude", CWD: "/tmp", HostID: "h",
		StartedAt: t0, LastEventAt: t0, Status: session.StateWaiting,
	}); err != nil {
		t.Fatal(err)
	}

	// A newer tool_use moves it to running.
	if _, err := st.UpdateStatus(ctx, "s", session.StateRunning, t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	// A stale notification (earlier ts) must be ignored, not rewind to waiting.
	if n, err := st.UpdateStatus(ctx, "s", session.StateWaiting, t0.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("stale update changed %d rows, want 0 (recency guard should skip it)", n)
	}
	if got, _ := st.CurrentStatus(ctx, "s"); got != session.StateRunning {
		t.Fatalf("stale event rewound state: got %q, want running", got)
	}

	// An equal-timestamp event still applies (ties resolve to last writer).
	if _, err := st.UpdateStatus(ctx, "s", session.StateIdle, t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.CurrentStatus(ctx, "s"); got != session.StateIdle {
		t.Fatalf("equal-ts event did not apply: got %q, want idle", got)
	}
}

// TestListSessionsReturnsIdentity guards the pid/pid_start/boot_id round trip:
// ListSessions once selected neither, so `sm status --json` and `sm ls` reported
// every session as pid 0 even when the fingerprint was captured.
func TestListSessionsReturnsIdentity(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	if err := st.CreateSession(ctx, session.Session{
		ID: "s", Agent: "claude", CWD: "/tmp", HostID: "h",
		StartedAt: now, LastEventAt: now, Status: session.StateWaiting,
		PID: 4242, PIDStart: 99, BootID: "boot",
	}); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 session, got %d", len(all))
	}
	got := all[0]
	if got.PID != 4242 || got.PIDStart != 99 || got.BootID != "boot" {
		t.Errorf("identity not returned: pid=%d start=%d boot=%q", got.PID, got.PIDStart, got.BootID)
	}
}

// TestListSessionsLastPrompt verifies LastPrompt is derived from the most
// recent user_prompt event, ignoring other event kinds and older prompts.
func TestListSessionsLastPrompt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	if err := st.CreateSession(ctx, session.Session{
		ID: "s", Agent: "claude", CWD: "/tmp", HostID: "h",
		StartedAt: now, LastEventAt: now, Status: session.StateWaiting,
	}); err != nil {
		t.Fatal(err)
	}
	// "with" has prompts, latest wins; "none" has only a non-prompt event.
	if err := st.CreateSession(ctx, session.Session{
		ID: "none", Agent: "claude", CWD: "/tmp", HostID: "h",
		StartedAt: now, LastEventAt: now, Status: session.StateWaiting,
	}); err != nil {
		t.Fatal(err)
	}

	ev := func(id, kind string, ts time.Time, payload map[string]any) {
		if err := st.AppendEvent(ctx, session.Event{
			SessionID: id, Kind: session.EventKind(kind), Timestamp: ts, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ev("s", "user_prompt", now.Add(-2*time.Minute), map[string]any{"prompt": "old prompt"})
	ev("s", "tool_use", now.Add(-1*time.Minute), map[string]any{"name": "Bash"})
	ev("s", "user_prompt", now, map[string]any{"prompt": "newest prompt"})
	ev("none", "tool_use", now, map[string]any{"name": "Read"})

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range all {
		got[s.ID] = s.LastPrompt
	}
	if got["s"] != "newest prompt" {
		t.Errorf("LastPrompt = %q, want %q", got["s"], "newest prompt")
	}
	if got["none"] != "" {
		t.Errorf("session with no user_prompt: LastPrompt = %q, want empty", got["none"])
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
	mk("done", session.StateFinished, 3)   // terminal (clean exit): must be skipped
	mk("idledead", session.StateIdle, 4)   // between turns but process gone: must be reaped
	mk("reaped", session.StateDead, 5)     // already dead (terminal): must be skipped

	// pids 2 and 4 are reported dead. The callback must never see the un-probeable
	// session or either terminal state (finished, dead).
	reaped, err := st.ReapStale(ctx, "h", func(s session.Session) bool {
		if s.PID == 0 || s.Status == session.StateFinished || s.Status == session.StateDead {
			t.Errorf("callback received un-probeable/terminal session %q (%s)", s.ID, s.Status)
		}
		return s.PID == 2 || s.PID == 4
	})
	if err != nil {
		t.Fatal(err)
	}
	reapedIDs := map[string]bool{}
	for _, s := range reaped {
		reapedIDs[s.ID] = true
		if s.Status != session.StateDead {
			t.Errorf("reaped session %q status = %q, want %q", s.ID, s.Status, session.StateDead)
		}
	}
	if len(reaped) != 2 || !reapedIDs["dead"] || !reapedIDs["idledead"] {
		t.Fatalf("want [dead idledead] reaped, got %+v", reaped)
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
	if got["idledead"] != session.StateDead {
		t.Errorf("idle-but-gone session: want %q, got %q", session.StateDead, got["idledead"])
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
