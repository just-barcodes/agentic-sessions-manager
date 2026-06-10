// Package bus wraps NATS pub/sub for session events. Subjects:
//
//	sm.session.<uuid>.event   — every event from a given session
//	sm.session.*.event        — wildcard the daemon subscribes to
package bus

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

const (
	SubjectEventAll = "sm.session.*.event"
	subjectEventFmt = "sm.session.%s.event"
)

// DefaultURL is the local embedded NATS server the daemon starts on launch.
const DefaultURL = "nats://127.0.0.1:4222"

// URL returns the bus URL clients should dial: $SM_BUS_URL when set (e.g. a
// tailnet address or an SSH-forwarded port), otherwise DefaultURL.
func URL() string {
	if v := os.Getenv("SM_BUS_URL"); v != "" {
		return v
	}
	return DefaultURL
}

// HostPort splits a bus URL into the host and port the daemon's embedded
// server binds. The port must be explicit so the daemon and its clients can
// never silently disagree about where the bus lives.
func HostPort(rawURL string) (string, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", 0, fmt.Errorf("bus url: %w", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return "", 0, fmt.Errorf("bus url %q: missing or invalid port", rawURL)
	}
	return u.Hostname(), port, nil
}

type Bus struct {
	conn *nats.Conn
}

// Connect dials the bus at url, authenticating with token when non-empty.
func Connect(url, token string) (*Bus, error) {
	c, err := nats.Connect(url, tokenOpts(token)...)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	return &Bus{conn: c}, nil
}

// ConnectInProcess connects directly to an in-process NATS server, bypassing
// TCP. The daemon uses this for its own subscription on the server it embeds.
func ConnectInProcess(srv nats.InProcessConnProvider, token string) (*Bus, error) {
	c, err := nats.Connect("", append(tokenOpts(token), nats.InProcessServer(srv))...)
	if err != nil {
		return nil, fmt.Errorf("nats connect in-process: %w", err)
	}
	return &Bus{conn: c}, nil
}

func tokenOpts(token string) []nats.Option {
	if token == "" {
		return nil
	}
	return []nats.Option{nats.Token(token)}
}

// LoadToken reads the bus auth token from path. A missing file is not an
// error: it returns "" so clients attempt an unauthenticated connection, which
// the server rejects with a clear authorization error if it requires one.
func LoadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// EnsureToken returns the token at path, generating and persisting a new one
// if the file does not exist. The daemon calls this on startup; the file is
// owner-only like the rest of the data dir.
func EnsureToken(path string) (string, error) {
	tok, err := LoadToken(path)
	if err != nil || tok != "" {
		return tok, err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok = hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func (b *Bus) Close() { b.conn.Close() }

// Subscribe invokes handler for every event on sm.session.*.event. The
// subscription stays active until the bus is drained (see Drain) or closed.
func (b *Bus) Subscribe(handler func(session.Event)) error {
	_, err := b.conn.Subscribe(SubjectEventAll, func(m *nats.Msg) {
		var e session.Event
		if err := json.Unmarshal(m.Data, &e); err != nil {
			log.Printf("bus: decode event: %v", err)
			return
		}
		handler(e)
	})
	return err
}

// Drain stops the subscriptions gracefully: it processes any in-flight messages
// so their handlers run to completion, then closes the connection. It blocks
// until draining finishes or timeout elapses, so callers can safely tear down
// resources the handlers touch (e.g. the store) once it returns.
func (b *Bus) Drain(timeout time.Duration) error {
	done := make(chan struct{})
	b.conn.SetClosedHandler(func(*nats.Conn) { close(done) })
	if err := b.conn.Drain(); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("bus drain timed out after %s", timeout)
	}
}

// Publish emits an event. SessionID may be empty for first-contact events
// (the daemon will assign a UUID).
func (b *Bus) Publish(e session.Event) error {
	subject := fmt.Sprintf(subjectEventFmt, sessionKey(e.SessionID))
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return b.conn.Publish(subject, data)
}

func sessionKey(id string) string {
	if id == "" {
		return "new"
	}
	return id
}
