// Package bus wraps NATS pub/sub for session events. Subjects:
//
//	sm.session.<uuid>.event   — every event from a given session
//	sm.session.*.event        — wildcard the daemon subscribes to
package bus

import (
	"context"
	"encoding/json"
	"fmt"

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

// Subscribe invokes handler for every event on sm.session.*.event until ctx is cancelled.
func (b *Bus) Subscribe(ctx context.Context, handler func(session.Event)) error {
	sub, err := b.conn.Subscribe(SubjectEventAll, func(m *nats.Msg) {
		var e session.Event
		if err := json.Unmarshal(m.Data, &e); err != nil {
			return // TODO: log decode failures
		}
		handler(e)
	})
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()
	return nil
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
