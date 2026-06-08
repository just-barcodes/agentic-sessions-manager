// Package bus wraps NATS pub/sub for session events. Subjects:
//
//	sm.session.<uuid>.event   — every event from a given session
//	sm.session.*.event        — wildcard the daemon subscribes to
package bus

import (
	"encoding/json"
	"fmt"
	"log"
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

type Bus struct {
	conn *nats.Conn
}

func Connect(url string) (*Bus, error) {
	c, err := nats.Connect(url)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	return &Bus{conn: c}, nil
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
