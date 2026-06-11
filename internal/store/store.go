// Package store is a thin SQLite wrapper for sessions and their events.
// All times are stored as Unix seconds.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

type Store struct {
	db *sql.DB
}

const (
	// busyTimeoutMS is how long SQLite waits on a locked database before
	// returning SQLITE_BUSY; the daemon and CLI open the same file concurrently.
	busyTimeoutMS = 5000
	// prefixMatchLimit caps the prefix lookup at two rows — enough to tell a
	// unique match from an ambiguous one without scanning further.
	prefixMatchLimit = 2
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id            TEXT PRIMARY KEY,
	agent         TEXT NOT NULL,
	native_id     TEXT NOT NULL DEFAULT '',
	cwd           TEXT NOT NULL,
	host_id       TEXT NOT NULL,
	started_at    INTEGER NOT NULL,
	last_event_at INTEGER NOT NULL,
	status        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id),
	ts         INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	payload    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_session_ts ON events(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_sessions_status   ON sessions(status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_native ON sessions(agent, native_id)
	WHERE native_id != '';
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite has no concurrent writer. Pin to a single connection and
	// enable WAL + a busy timeout so the daemon and CLI processes, which each
	// open their own *Store against the same file, don't collide with SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS)} {
		if _, err := db.ExecContext(context.Background(), pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set pragma: %w", err)
		}
	}
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// The database holds prompt text and cwd paths; keep it owner-only.
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = db.Close()
		return nil, fmt.Errorf("chmod db: %w", err)
	}
	return &Store{db: db}, nil
}

// XDGDir resolves an XDG base directory for sm: <$envVar>/sm when the variable
// is set, otherwise <home>/<fallback>/sm. Shared by the store and the daemon so
// the data and state directories resolve through one rule.
func XDGDir(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" {
		return filepath.Join(v, "sm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback, "sm")
}

// DataDir returns the sm data directory, honoring XDG_DATA_HOME and falling back
// to ~/.local/share/sm. Both the daemon and the CLI resolve the database through
// here so they always agree on its location.
func DataDir() string {
	return XDGDir("XDG_DATA_HOME", ".local/share")
}

// DefaultDBPath is the path to the sm database within DataDir.
func DefaultDBPath() string { return filepath.Join(DataDir(), "sm.db") }

// BusTokenPath is the path to the bus auth token within DataDir, so the daemon
// and clients always agree on its location.
func BusTokenPath() string { return filepath.Join(DataDir(), "bus-token") }

// latestPromptSQL computes a session's most recent user_prompt text from the
// events table (newest ts wins, event id breaks ties). Correlated on
// sessions.id so it slots into any statement over sessions; binds one arg,
// the user_prompt event kind.
const latestPromptSQL = `COALESCE((
	SELECT json_extract(e.payload, '$.prompt')
	FROM events e
	WHERE e.session_id = sessions.id AND e.kind = ?
	ORDER BY e.ts DESC, e.id DESC LIMIT 1
), '')`

// migrate applies additive column migrations. Re-running is safe: adding a
// column that already exists errors with "duplicate column name", which we
// ignore so existing databases pick up the new columns on next open.
func migrate(db *sql.DB) error {
	alters := []string{
		`ALTER TABLE sessions ADD COLUMN pid INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN pid_start INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sessions ADD COLUMN boot_id TEXT NOT NULL DEFAULT ''`,
	}
	for _, q := range alters {
		if _, err := db.ExecContext(context.Background(), q); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// last_prompt is a denormalized cache of the events table. The first time the
	// column is added, backfill it so sessions recorded before the upgrade keep
	// their prompts; on later opens the ALTER fails as a duplicate and the
	// backfill is skipped.
	if _, err := db.ExecContext(context.Background(),
		`ALTER TABLE sessions ADD COLUMN last_prompt TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
		return nil
	}
	_, err := db.ExecContext(context.Background(),
		`UPDATE sessions SET last_prompt = `+latestPromptSQL,
		string(session.EventUserPrompt))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateSession(ctx context.Context, sess session.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, agent, native_id, cwd, host_id, started_at, last_event_at, status, pid, pid_start, boot_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Agent, sess.NativeID, sess.CWD, sess.HostID,
		sess.StartedAt.Unix(), sess.LastEventAt.Unix(), string(sess.Status),
		sess.PID, sess.PIDStart, sess.BootID)
	return err
}

// FindSessionByNative resolves the daemon-assigned UUID for an agent's native
// session id. Returns "" if no mapping exists.
func (s *Store) FindSessionByNative(ctx context.Context, agent, nativeID string) (string, error) {
	if nativeID == "" {
		return "", nil
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE agent = ? AND native_id = ?`,
		agent, nativeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// UpdateStatus applies a state transition, but only if the event is at least as
// recent as the last one already applied. The recency guard (ts >= last_event_at)
// drops events that arrive out of order — e.g. a stale notification landing after
// a newer tool_use — so a late event cannot rewind the session's state. It
// returns the number of rows changed (0 when the guard skipped the update) so
// callers can tell whether the transition actually took effect.
func (s *Store) UpdateStatus(ctx context.Context, id string, status session.State, ts time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET status = ?, last_event_at = ? WHERE id = ? AND ? >= last_event_at`,
		string(status), ts.Unix(), id, ts.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CurrentStatus returns the session's current state. It returns "" with no error
// when the session is unknown, so callers can treat that as "no current state".
func (s *Store) CurrentStatus(ctx context.Context, id string) (session.State, error) {
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM sessions WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return session.State(status), err
}

func (s *Store) AppendEvent(ctx context.Context, e session.Event) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(session_id, ts, kind, payload) VALUES(?,?,?,?)`,
		e.SessionID, e.Timestamp.Unix(), string(e.Kind), string(payload)); err != nil {
		return err
	}
	// Denormalize the latest user prompt onto the session row so ListSessions can
	// read it directly instead of scanning the events table once per session.
	// Recomputing from events (newest ts wins) rather than taking e's payload
	// means an out-of-order prompt cannot overwrite a newer one, and the shared
	// transaction keeps the cached column consistent with the insert.
	if e.Kind == session.EventUserPrompt {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET last_prompt = `+latestPromptSQL+` WHERE id = ?`,
			string(session.EventUserPrompt), e.SessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListSessions returns sessions ordered most-recently-active first. When
// includeFinished is false, sessions in the finished state are omitted.
func (s *Store) ListSessions(ctx context.Context, includeFinished bool) ([]session.Session, error) {
	query := `
		SELECT s.id, s.agent, s.native_id, s.cwd, s.host_id, s.started_at, s.last_event_at, s.status, s.pid, s.pid_start, s.boot_id, s.last_prompt
		FROM sessions s`
	var args []any
	if !includeFinished {
		clause, targs := terminalNotIn("s.status")
		query += ` WHERE ` + clause
		args = append(args, targs...)
	}
	query += ` ORDER BY s.last_event_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []session.Session
	for rows.Next() {
		var lastPrompt string
		sess, err := scanSession(rows, &lastPrompt)
		if err != nil {
			return nil, err
		}
		sess.LastPrompt = lastPrompt
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) CountByStatus(ctx context.Context, status session.State) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE status = ?`, string(status)).Scan(&n)
	return n, err
}

// ReapStale marks non-terminal sessions on hostID dead when isDead reports their
// agent process is gone. Every state except the two terminal ones (finished —
// clean exit — and dead — already reaped) is probed, so a session that died
// while running, waiting, idle, or failed is corrected. Only sessions with a
// captured pid are probed; the rest are left untouched (un-probeable). The
// session's last_event_at is preserved so the row still reflects when the agent
// was actually last seen. Returns the reaped sessions (with Status set to dead)
// so callers can react.
func (s *Store) ReapStale(ctx context.Context, hostID string, isDead func(session.Session) bool) ([]session.Session, error) {
	clause, targs := terminalNotIn("status")
	qargs := append([]any{hostID}, targs...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, cwd, host_id, last_event_at, pid, pid_start, boot_id
		FROM sessions
		WHERE host_id = ? AND pid != 0 AND `+clause,
		qargs...)
	if err != nil {
		return nil, err
	}
	var stale []session.Session
	for rows.Next() {
		var sess session.Session
		var lastEventAt int64
		if err := rows.Scan(&sess.ID, &sess.Agent, &sess.CWD, &sess.HostID, &lastEventAt,
			&sess.PID, &sess.PIDStart, &sess.BootID); err != nil {
			rows.Close()
			return nil, err
		}
		sess.LastEventAt = time.Unix(lastEventAt, 0)
		stale = append(stale, sess)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // release before issuing writes on the same connection

	var reaped []session.Session
	for _, sess := range stale {
		if !isDead(sess) {
			continue
		}
		// A concurrent newer event can win the recency guard between our SELECT
		// and this UPDATE, leaving the row in a different terminal state. Only
		// report the session as reaped when the UPDATE actually changed a row.
		n, err := s.UpdateStatus(ctx, sess.ID, session.StateDead, sess.LastEventAt)
		if err != nil {
			return reaped, err
		}
		if n == 0 {
			continue
		}
		sess.Status = session.StateDead
		reaped = append(reaped, sess)
	}
	return reaped, nil
}

// RemoteReapTTL is how long a session on another host may go without events
// before it is presumed dead. Remote agent processes cannot be probed via
// /proc, so event recency is the only liveness signal; the TTL is generous
// because an idle-at-prompt session legitimately emits no events for hours.
const RemoteReapTTL = 24 * time.Hour

// ReapRemoteStale marks non-terminal sessions on hosts other than localHostID
// dead when their last event is older than cutoff. The TTL counterpart of
// ReapStale for sessions the local /proc reaper cannot probe. last_event_at is
// preserved so the row still reflects when the agent was actually last seen.
// Returns the reaped sessions (with Status set to dead) so callers can react.
func (s *Store) ReapRemoteStale(ctx context.Context, localHostID string, cutoff time.Time) ([]session.Session, error) {
	clause, targs := terminalNotIn("status")
	qargs := append([]any{localHostID, cutoff.Unix()}, targs...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, cwd, host_id, last_event_at
		FROM sessions
		WHERE host_id != ? AND last_event_at < ? AND `+clause,
		qargs...)
	if err != nil {
		return nil, err
	}
	var stale []session.Session
	for rows.Next() {
		var sess session.Session
		var lastEventAt int64
		if err := rows.Scan(&sess.ID, &sess.Agent, &sess.CWD, &sess.HostID, &lastEventAt); err != nil {
			rows.Close()
			return nil, err
		}
		sess.LastEventAt = time.Unix(lastEventAt, 0)
		stale = append(stale, sess)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // release before issuing writes on the same connection

	var reaped []session.Session
	for _, sess := range stale {
		// Same recency guard as ReapStale: a newer event arriving between the
		// SELECT and this UPDATE wins, so only report actually-changed rows.
		n, err := s.UpdateStatus(ctx, sess.ID, session.StateDead, sess.LastEventAt)
		if err != nil {
			return reaped, err
		}
		if n == 0 {
			continue
		}
		sess.Status = session.StateDead
		reaped = append(reaped, sess)
	}
	return reaped, nil
}

// terminalNotIn builds a "<col> NOT IN (?, ?)" clause plus the bind args for the
// terminal states, so the rule for which sessions are hidden/skipped lives in
// one place and both ListSessions and ReapStale stay in sync.
func terminalNotIn(col string) (clause string, args []any) {
	terminal := session.TerminalStates()
	ph := make([]string, len(terminal))
	args = make([]any, len(terminal))
	for i, t := range terminal {
		ph[i] = "?"
		args[i] = string(t)
	}
	return col + " NOT IN (" + strings.Join(ph, ", ") + ")", args
}

// GetSession resolves a session by id prefix and returns it along with its
// most recent events ordered oldest-first. Errors if the prefix matches zero
// or more than one session.
func (s *Store) GetSession(ctx context.Context, idPrefix string, limit int) (session.Session, []session.Event, error) {
	if idPrefix == "" {
		return session.Session{}, nil, errors.New("empty session id")
	}
	sess, err := s.resolveByPrefix(ctx, idPrefix)
	if err != nil {
		return session.Session{}, nil, err
	}
	events, err := s.recentEvents(ctx, sess.ID, limit)
	if err != nil {
		return sess, nil, err
	}
	return sess, events, nil
}

// ResolveSessionID returns the full session UUID matching the given prefix,
// or an error if the prefix matches zero or more than one session.
func (s *Store) ResolveSessionID(ctx context.Context, idPrefix string) (string, error) {
	if idPrefix == "" {
		return "", errors.New("empty session id")
	}
	sess, err := s.resolveByPrefix(ctx, idPrefix)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

func (s *Store) resolveByPrefix(ctx context.Context, idPrefix string) (session.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, native_id, cwd, host_id, started_at, last_event_at, status, pid, pid_start, boot_id
		FROM sessions WHERE id LIKE ? LIMIT ?`, idPrefix+"%", prefixMatchLimit)
	if err != nil {
		return session.Session{}, err
	}
	defer rows.Close()

	var found []session.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return session.Session{}, err
		}
		found = append(found, sess)
	}
	if err := rows.Err(); err != nil {
		return session.Session{}, err
	}
	switch len(found) {
	case 0:
		return session.Session{}, fmt.Errorf("no session matches %q", idPrefix)
	case 1:
		return found[0], nil
	default:
		return session.Session{}, fmt.Errorf("ambiguous prefix %q: matches multiple sessions", idPrefix)
	}
}

// scanSession reads the eleven core session columns from the current row, in the
// column order every full-session SELECT uses, and applies the Unix→time and
// status conversions. Callers selecting trailing columns (e.g. ListSessions'
// last_prompt) pass their scan destinations as extra; they are appended to the
// single Scan call.
func scanSession(rows *sql.Rows, extra ...any) (session.Session, error) {
	var sess session.Session
	var startedAt, lastEventAt int64
	var status string
	dest := append([]any{
		&sess.ID, &sess.Agent, &sess.NativeID, &sess.CWD, &sess.HostID,
		&startedAt, &lastEventAt, &status, &sess.PID, &sess.PIDStart, &sess.BootID,
	}, extra...)
	if err := rows.Scan(dest...); err != nil {
		return session.Session{}, err
	}
	sess.StartedAt = time.Unix(startedAt, 0)
	sess.LastEventAt = time.Unix(lastEventAt, 0)
	sess.Status = session.State(status)
	return sess, nil
}

func (s *Store) recentEvents(ctx context.Context, sessionID string, limit int) ([]session.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, kind, payload FROM events
		WHERE session_id = ? ORDER BY ts DESC, id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []session.Event
	for rows.Next() {
		var ts int64
		var kind, payloadJSON string
		if err := rows.Scan(&ts, &kind, &payloadJSON); err != nil {
			return nil, err
		}
		e := session.Event{
			SessionID: sessionID,
			Kind:      session.EventKind(kind),
			Timestamp: time.Unix(ts, 0),
		}
		if payloadJSON != "" && payloadJSON != "null" {
			_ = json.Unmarshal([]byte(payloadJSON), &e.Payload)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to oldest-first for chronological display.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, nil
}
