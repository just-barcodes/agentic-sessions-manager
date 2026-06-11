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
	// A straggler delivered late with an older timestamp (clock step, replay)
	// must not overwrite the newest prompt.
	ev("s", "user_prompt", now.Add(-3*time.Minute), map[string]any{"prompt": "stale straggler"})

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

	// pids 2 and 4 are reported dead. ReapStale must offer isDead only the
	// probeable, non-terminal sessions: never the un-probeable one (pid 0) or
	// either terminal state (finished, dead). We record which IDs the callback
	// receives and assert on them below — checking s.Status inside the callback
	// would be a no-op because ReapStale's SELECT never loads the status column.
	probed := map[string]bool{}
	reaped, err := st.ReapStale(ctx, "h", func(s session.Session) bool {
		probed[s.ID] = true
		return s.PID == 2 || s.PID == 4
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"noident", "done", "reaped"} {
		if probed[id] {
			t.Errorf("isDead was called for %q; it should be filtered out before probing", id)
		}
	}
	for _, id := range []string{"alive", "dead", "idledead"} {
		if !probed[id] {
			t.Errorf("isDead was not called for probeable session %q", id)
		}
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

func TestReapRemoteStale(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	mk := func(id, host string, status session.State, last time.Time) {
		if err := st.CreateSession(ctx, session.Session{
			ID: id, Agent: "claude", CWD: "/tmp", HostID: host,
			StartedAt: last, LastEventAt: last, Status: status,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("remote-old", "laptop", session.StateWaiting, old)   // must be reaped
	mk("remote-fresh", "laptop", session.StateRunning, now) // recent events: kept
	mk("local-old", "h", session.StateRunning, old)         // local host: ReapStale's job, not TTL's
	mk("remote-done", "laptop", session.StateFinished, old) // terminal: must be skipped
	mk("remote-idle-old", "laptop", session.StateIdle, old) // idle counts too: no liveness signal
	mk("remote-reaped", "laptop", session.StateDead, old)   // already dead: must be skipped

	cutoff := now.Add(-time.Hour)
	reaped, err := st.ReapRemoteStale(ctx, "h", cutoff)
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
	if len(reaped) != 2 || !reapedIDs["remote-old"] || !reapedIDs["remote-idle-old"] {
		t.Fatalf("want [remote-old remote-idle-old] reaped, got %+v", reaped)
	}

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]session.State{}
	for _, s := range all {
		got[s.ID] = s.Status
	}
	if got["remote-old"] != session.StateDead {
		t.Errorf("remote-old: want %q, got %q", session.StateDead, got["remote-old"])
	}
	if got["remote-fresh"] != session.StateRunning {
		t.Errorf("remote session with recent events wrongly reaped to %q", got["remote-fresh"])
	}
	if got["local-old"] != session.StateRunning {
		t.Errorf("local session must not be TTL-reaped, got %q", got["local-old"])
	}
	if got["remote-done"] != session.StateFinished {
		t.Errorf("terminal remote session changed to %q", got["remote-done"])
	}

	// last_event_at is preserved: the row still says when the agent was last seen.
	sess, _, err := st.GetSession(ctx, "remote-old", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sess.LastEventAt.Unix() != old.Unix() {
		t.Errorf("last_event_at = %v, want preserved %v", sess.LastEventAt.Unix(), old.Unix())
	}
}

func TestGetSession(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	if err := st.CreateSession(ctx, session.Session{
		ID: "abcdef01", Agent: "claude", CWD: "/tmp", HostID: "h",
		StartedAt: now, LastEventAt: now, Status: session.StateRunning,
	}); err != nil {
		t.Fatal(err)
	}

	// Five events at increasing timestamps. recentEvents keeps the newest N and
	// returns them oldest-first, so GetSession with limit 3 must yield the last
	// three kinds in chronological order.
	kinds := []session.EventKind{
		session.EventSessionStart, session.EventUserPrompt, session.EventToolUse,
		session.EventStop, session.EventSessionEnd,
	}
	for i, k := range kinds {
		if err := st.AppendEvent(ctx, session.Event{
			SessionID: "abcdef01", Kind: k, Timestamp: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	sess, events, err := st.GetSession(ctx, "abc", 3)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "abcdef01" {
		t.Errorf("resolved session ID = %q, want %q", sess.ID, "abcdef01")
	}
	want := []session.EventKind{session.EventToolUse, session.EventStop, session.EventSessionEnd}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d (limit should keep the newest 3)", len(events), len(want))
	}
	for i, k := range want {
		if events[i].Kind != k {
			t.Errorf("events[%d].Kind = %q, want %q (oldest-first ordering)", i, events[i].Kind, k)
		}
	}

	if _, _, err := st.GetSession(ctx, "", 10); err == nil {
		t.Error("empty prefix: want error, got nil")
	}
}

func TestFindSessionByNative(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	if err := st.CreateSession(ctx, session.Session{
		ID: "uuid-1", Agent: "claude", NativeID: "claude-native-123", CWD: "/tmp",
		HostID: "h", StartedAt: now, LastEventAt: now, Status: session.StateRunning,
	}); err != nil {
		t.Fatal(err)
	}

	// A known (agent, native_id) resolves to the daemon-assigned UUID.
	id, err := st.FindSessionByNative(ctx, "claude", "claude-native-123")
	if err != nil {
		t.Fatal(err)
	}
	if id != "uuid-1" {
		t.Errorf("FindSessionByNative = %q, want %q", id, "uuid-1")
	}

	// Unknown native id, right native but wrong agent, and empty native id must
	// all resolve to "" — the lookup is scoped by both agent and native_id, so a
	// mismatch creates a fresh session rather than colliding with this one.
	misses := []struct{ agent, native string }{
		{"claude", "unknown-native"},
		{"opencode", "claude-native-123"},
		{"claude", ""},
	}
	for _, c := range misses {
		got, err := st.FindSessionByNative(ctx, c.agent, c.native)
		if err != nil {
			t.Fatalf("FindSessionByNative(%q, %q): %v", c.agent, c.native, err)
		}
		if got != "" {
			t.Errorf("FindSessionByNative(%q, %q) = %q, want \"\"", c.agent, c.native, got)
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

// TestMigrateBackfillsLastPrompt guards upgrades: a database from before the
// last_prompt column existed must have it populated from the existing
// user_prompt events, or `sm ls` would show blank prompts for every
// pre-upgrade session until a new prompt arrived.
func TestMigrateBackfillsLastPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sm.db")

	// Stand up the pre-last_prompt schema with a session and its events, the way
	// an older sm would have left it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, agent TEXT NOT NULL, native_id TEXT NOT NULL DEFAULT '',
		cwd TEXT NOT NULL, host_id TEXT NOT NULL, started_at INTEGER NOT NULL,
		last_event_at INTEGER NOT NULL, status TEXT NOT NULL,
		pid INTEGER NOT NULL DEFAULT 0, pid_start INTEGER NOT NULL DEFAULT 0,
		boot_id TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL REFERENCES sessions(id),
		ts INTEGER NOT NULL, kind TEXT NOT NULL, payload TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES
		('with','claude','','/tmp','h',0,0,'running',0,0,''),
		('none','claude','','/tmp','h',0,0,'running',0,0,'')`); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []struct {
		ts            int
		kind, payload string
	}{
		{100, "user_prompt", `{"prompt":"first"}`},
		{200, "user_prompt", `{"prompt":"latest"}`},
		{300, "tool_use", `{"name":"Bash"}`},
	} {
		if _, err := db.Exec(
			`INSERT INTO events(session_id, ts, kind, payload) VALUES('with',?,?,?)`,
			ev.ts, ev.kind, ev.payload); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-last_prompt DB: %v", err)
	}
	defer st.Close()

	all, err := st.ListSessions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range all {
		got[s.ID] = s.LastPrompt
	}
	if got["with"] != "latest" {
		t.Errorf("backfilled LastPrompt = %q, want %q", got["with"], "latest")
	}
	if got["none"] != "" {
		t.Errorf("session with no prompts: LastPrompt = %q, want empty", got["none"])
	}
}
