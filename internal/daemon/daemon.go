// Package daemon ties the bus, store, and alert sinks together. It is the
// long-running process behind `sm daemon`.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

// sweepInterval is how often the daemon reaps sessions whose agent process has
// died without a clean exit. Liveness checks are cheap (a few /proc reads each).
const sweepInterval = 30 * time.Second

func Run(_ []string) error {
	dataDir := xdgDir("XDG_DATA_HOME", ".local/share")
	stateDir := xdgDir("XDG_STATE_HOME", ".local/state")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(dataDir, "sm.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	ns, err := startEmbeddedNATS()
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
			alert.NotifySend{},
			alert.CountFile{
				Path: filepath.Join(stateDir, "waiting-count"),
				Count: func() (int, error) {
					return st.CountByStatus(context.Background(), session.StateWaiting)
				},
			},
		},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := b.Subscribe(ctx, h.handle); err != nil {
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

func (h *handler) handle(e session.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ctx := context.Background()

	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if err := h.resolveSession(ctx, &e); err != nil {
		return // TODO: log
	}
	_ = h.store.AppendEvent(ctx, e)

	next := session.NextState(e.Kind)
	if next == "" {
		return
	}
	_ = h.store.UpdateStatus(ctx, e.SessionID, next, e.Timestamp)

	sess := session.Session{
		ID:       e.SessionID,
		Agent:    e.Agent,
		NativeID: e.NativeID,
		CWD:      str(e.Payload["cwd"]),
		HostID:   h.hostID,
		Status:   next,
	}
	for _, sink := range h.sinks {
		_ = sink.OnStateChange(sess)
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
	reaped, err := h.store.ReapStale(ctx, h.hostID, func(s session.Session) bool {
		return !liveness.Alive(liveness.Identity{PID: s.PID, Start: s.PIDStart, BootID: s.BootID})
	})
	if err != nil {
		return // TODO: log
	}
	for _, sess := range reaped {
		for _, sink := range h.sinks {
			_ = sink.OnStateChange(sess)
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
		CWD:         str(e.Payload["cwd"]),
		HostID:      h.hostID,
		StartedAt:   e.Timestamp,
		LastEventAt: e.Timestamp,
		Status:      session.StateRunning,
		PID:         e.PID,
		PIDStart:    e.PIDStart,
		BootID:      e.BootID,
	})
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// startEmbeddedNATS boots an in-process NATS server bound to localhost so the
// daemon owns the bus end-to-end. Logging is suppressed; bind failures surface
// as a ReadyForConnections timeout.
func startEmbeddedNATS() (*natsserver.Server, error) {
	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   4222,
		NoLog:  true,
		NoSigs: true, // the daemon owns signal handling
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("create nats server: %w", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		return nil, errors.New("nats server not ready (port 4222 in use?)")
	}
	return ns, nil
}

func xdgDir(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" {
		return filepath.Join(v, "sm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback, "sm")
}
