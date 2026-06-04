// Package daemon ties the bus, store, and alert sinks together. It is the
// long-running process behind `sm daemon`.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/just-barcodes/agentic-sessions-manager/internal/alert"
	"github.com/just-barcodes/agentic-sessions-manager/internal/bus"
	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
	"github.com/just-barcodes/agentic-sessions-manager/internal/store"
)

const (
	// sweepInterval is how often the daemon reaps sessions whose agent process
	// has died without a clean exit. Liveness checks are cheap (a few /proc reads each).
	sweepInterval = 30 * time.Second

	countTimeout     = 2 * time.Second // bound the waiting-count query for an alert sink
	storeOpTimeout   = 5 * time.Second // bound a single event's store operations
	natsReadyTimeout = 5 * time.Second // how long to wait for the embedded NATS server to accept connections
)

func Run(_ []string) error {
	dataDir := store.DataDir()
	stateDir := store.XDGDir("XDG_STATE_HOME", ".local/state")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	st, err := store.Open(store.DefaultDBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	ns, err := startEmbeddedNATS("127.0.0.1", 4222)
	if err != nil {
		return err
	}
	defer func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	}()

	b, err := bus.Connect(bus.DefaultURL)
	if err != nil {
		return err
	}
	defer b.Close()

	hostname, _ := os.Hostname()

	h := &handler{
		store:  st,
		hostID: hostname,
		sinks: []alert.Sink{
			alert.CountFile{
				Path: filepath.Join(stateDir, "waiting-count"),
				Count: func() (int, error) {
					ctx, cancel := context.WithTimeout(context.Background(), countTimeout)
					defer cancel()
					return st.CountByStatus(ctx, session.StateWaiting)
				},
			},
		},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := b.Subscribe(ctx, func(e session.Event) { h.handle(ctx, e) }); err != nil {
		return err
	}
	go h.sweepLoop(ctx)
	<-ctx.Done()
	return nil
}

// handler is the single point where bus events become persisted state changes.
// Serialised with a mutex — the event rate is tiny so there is no benefit to
// concurrent processing, and ordering matters for state transitions.
type handler struct {
	mu     sync.Mutex
	store  *store.Store
	hostID string
	sinks  []alert.Sink
}

func (h *handler) handle(ctx context.Context, e session.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, storeOpTimeout)
	defer cancel()

	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if err := h.resolveSession(ctx, &e); err != nil {
		log.Printf("daemon: resolve session: %v", err)
		return
	}
	if err := h.store.AppendEvent(ctx, e); err != nil {
		log.Printf("daemon: append event: %v", err)
	}
	logUnclassifiedNotification(e)

	cur, _ := h.store.CurrentStatus(ctx, e.SessionID)
	next := session.Transition(cur, e)
	if next == "" {
		return
	}
	if _, err := h.store.UpdateStatus(ctx, e.SessionID, next, e.Timestamp); err != nil {
		log.Printf("daemon: update status: %v", err)
		return // don't fan out a state change we failed to persist
	}

	sess := session.Session{
		ID:       e.SessionID,
		Agent:    e.Agent,
		NativeID: e.NativeID,
		CWD:      asString(e.Payload["cwd"]),
		HostID:   h.hostID,
		Status:   next,
	}
	for _, sink := range h.sinks {
		if err := sink.OnStateChange(sess); err != nil {
			log.Printf("daemon: sink: %v", err)
		}
	}
}

// sweepLoop reaps sessions whose agent process has died without a clean stop.
// It sweeps once immediately — clearing sessions left running across a daemon
// restart or reboot — then on every tick until ctx is cancelled.
func (h *handler) sweepLoop(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	h.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sweep(ctx)
		}
	}
}

// sweep marks dead sessions and notifies sinks so derived state (e.g. the
// waiting-count file) reflects the reaping. Shares the handler mutex with
// handle so reaping and event processing never interleave.
func (h *handler) sweep(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reaped, err := h.store.ReapStale(ctx, h.hostID, liveness.IsProcessDead)
	if err != nil {
		log.Printf("daemon: reap stale: %v", err)
		return
	}
	for _, sess := range reaped {
		for _, sink := range h.sinks {
			if err := sink.OnStateChange(sess); err != nil {
				log.Printf("daemon: sink: %v", err)
			}
		}
	}
}

// resolveSession populates e.SessionID, creating the session row on first
// contact. Lookup order: explicit SessionID → (agent, native_id) → new UUID.
func (h *handler) resolveSession(ctx context.Context, e *session.Event) error {
	if e.SessionID != "" {
		return nil
	}
	if e.NativeID != "" {
		existing, err := h.store.FindSessionByNative(ctx, e.Agent, e.NativeID)
		if err != nil {
			return err
		}
		if existing != "" {
			e.SessionID = existing
			return nil
		}
	}
	e.SessionID = newID()
	return h.store.CreateSession(ctx, session.Session{
		ID:          e.SessionID,
		Agent:       e.Agent,
		NativeID:    e.NativeID,
		CWD:         asString(e.Payload["cwd"]),
		HostID:      h.hostID,
		StartedAt:   e.Timestamp,
		LastEventAt: e.Timestamp,
		Status:      session.StateIdle,
		PID:         e.PID,
		PIDStart:    e.PIDStart,
		BootID:      e.BootID,
	})
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// logUnclassifiedNotification surfaces a Notification the hook could not map to a
// sub-type (empty Notify) but that still carried a message — i.e. wording the
// classifier in hook.claudeNotify doesn't recognise yet. These fall back to
// waiting, so logging the raw message here (durably, via journald) is how we
// catch Claude changing or localizing its notification text before it silently
// mislabels sessions. Typeless, messageless notifications are skipped: there is
// nothing to learn from them.
func logUnclassifiedNotification(e session.Event) {
	if e.Kind != session.EventNotification || e.Notify != "" {
		return
	}
	if msg := asString(e.Payload["message"]); msg != "" {
		log.Printf("daemon: unclassified notification (defaulting to waiting): %q", msg)
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// startEmbeddedNATS boots an in-process NATS server bound to localhost so the
// daemon owns the bus end-to-end. Logging is suppressed; bind failures surface
// as a ReadyForConnections timeout.
func startEmbeddedNATS(host string, port int) (*natsserver.Server, error) {
	opts := &natsserver.Options{
		Host:   host,
		Port:   port,
		NoLog:  true,
		NoSigs: true, // the daemon owns signal handling
	}
	if port <= 1024 || port > 65535 {
		return nil, fmt.Errorf("create nats server: invalid port number")
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("create nats server: %w", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(natsReadyTimeout) {
		ns.Shutdown()
		return nil, errors.New("nats server not ready (port in use?)")
	}
	return ns, nil
}
