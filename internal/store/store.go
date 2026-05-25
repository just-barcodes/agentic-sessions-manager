// Package store is a thin SQLite wrapper for sessions and their events.
// All times are stored as Unix seconds.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

type Store struct {
	db *sql.DB
}

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
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateSession(ctx context.Context, sess session.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, agent, native_id, cwd, host_id, started_at, last_event_at, status)
		VALUES(?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Agent, sess.NativeID, sess.CWD, sess.HostID,
		sess.StartedAt.Unix(), sess.LastEventAt.Unix(), string(sess.Status))
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

func (s *Store) UpdateStatus(ctx context.Context, id string, status session.State, ts time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET status = ?, last_event_at = ? WHERE id = ?`,
		string(status), ts.Unix(), id)
	return err
}

func (s *Store) AppendEvent(ctx context.Context, e session.Event) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO events(session_id, ts, kind, payload) VALUES(?,?,?,?)`,
		e.SessionID, e.Timestamp.Unix(), string(e.Kind), string(payload))
	return err
}

// ListSessions returns sessions ordered most-recently-active first. When
// includeFinished is false, sessions in the finished state are omitted.
func (s *Store) ListSessions(ctx context.Context, includeFinished bool) ([]session.Session, error) {
	query := `
		SELECT id, agent, native_id, cwd, host_id, started_at, last_event_at, status
		FROM sessions`
	var args []any
	if !includeFinished {
		query += ` WHERE status != ?`
		args = append(args, string(session.StateFinished))
	}
	query += ` ORDER BY last_event_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []session.Session
	for rows.Next() {
		var sess session.Session
		var startedAt, lastEventAt int64
		var status string
		if err := rows.Scan(&sess.ID, &sess.Agent, &sess.NativeID, &sess.CWD, &sess.HostID,
			&startedAt, &lastEventAt, &status); err != nil {
			return nil, err
		}
		sess.StartedAt = time.Unix(startedAt, 0)
		sess.LastEventAt = time.Unix(lastEventAt, 0)
		sess.Status = session.State(status)
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
		SELECT id, agent, native_id, cwd, host_id, started_at, last_event_at, status
		FROM sessions WHERE id LIKE ? LIMIT 2`, idPrefix+"%")
	if err != nil {
		return session.Session{}, err
	}
	defer rows.Close()

	var found []session.Session
	for rows.Next() {
		var sess session.Session
		var startedAt, lastEventAt int64
		var status string
		if err := rows.Scan(&sess.ID, &sess.Agent, &sess.NativeID, &sess.CWD, &sess.HostID,
			&startedAt, &lastEventAt, &status); err != nil {
			return session.Session{}, err
		}
		sess.StartedAt = time.Unix(startedAt, 0)
		sess.LastEventAt = time.Unix(lastEventAt, 0)
		sess.Status = session.State(status)
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
