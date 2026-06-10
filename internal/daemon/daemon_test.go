package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/alert"
	"github.com/just-barcodes/agentic-sessions-manager/internal/bus"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
	"github.com/just-barcodes/agentic-sessions-manager/internal/store"
)

// TestSweepReapsAndRefreshesCount verifies the sweep both marks a dead session
// and fires the sinks, so the waiting-count file (read by status bars) no longer
// counts a session whose agent has gone.
func TestSweepReapsAndRefreshesCount(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	const host = "h"

	// A waiting session whose process is provably gone: a boot id that cannot
	// match the current boot makes liveness.Alive return false deterministically.
	if err := st.CreateSession(ctx, session.Session{
		ID: "dead", Agent: "claude", CWD: "/tmp", HostID: host,
		StartedAt: now, LastEventAt: now, Status: session.StateWaiting,
		PID: 4242, PIDStart: 1, BootID: "not-the-current-boot",
	}); err != nil {
		t.Fatal(err)
	}

	countPath := filepath.Join(dir, "waiting-count")
	if err := os.WriteFile(countPath, []byte("1\n"), 0o644); err != nil { // stale count
		t.Fatal(err)
	}
	h := &handler{
		store:  st,
		hostID: host,
		sinks: []alert.Sink{alert.CountFile{
			Path:  countPath,
			Count: func() (int, error) { return st.CountByStatus(ctx, session.StateWaiting) },
		}},
	}

	h.sweep()

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != session.StateDead {
		t.Fatalf("sweep did not mark the session dead: %+v", all)
	}

	b, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("count file not written: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "0" {
		t.Errorf("waiting-count = %q after sweep, want 0", got)
	}
}

// TestHandleAnswerResumesRunning reproduces the reported bug: the agent asks a
// question (→ waiting) and the user answers, which must move the session back to
// running rather than leaving it stuck in waiting. A stale idle ping arriving
// afterwards must not rewind it.
func TestHandleAnswerResumesRunning(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	h := &handler{store: st, hostID: "h"}
	t0 := time.Unix(1_000_000, 0)
	notif := func(typ session.NotifyType, dt time.Duration) session.Event {
		return session.Event{
			Agent: "claude", NativeID: "n", Kind: session.EventNotification,
			Timestamp: t0.Add(dt),
			Notify:    typ,
		}
	}

	h.handle(notif(session.NotifyElicitDialog, 0))            // agent asks
	h.handle(notif(session.NotifyElicitResp, time.Second))    // user answers
	h.handle(notif(session.NotifyIdle, 500*time.Millisecond)) // stale idle ping (older ts)

	all, err := st.ListSessions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 session, got %d", len(all))
	}
	if all[0].Status != session.StateRunning {
		t.Fatalf("after answering, status = %q, want running", all[0].Status)
	}
}

// TestEmbeddedNATSHonorsBusURL verifies the daemon and its clients agree on
// SM_BUS_URL: the embedded server bound to the host/port parsed from bus.URL()
// accepts a token-auth client dialing that same URL. The pick-then-bind free
// port has a TOCTOU window, so a stolen port is retried rather than failed.
func TestEmbeddedNATSHonorsBusURL(t *testing.T) {
	const token = "test-token"
	var lastErr error
	for range 3 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		t.Setenv("SM_BUS_URL", fmt.Sprintf("nats://127.0.0.1:%d", port))

		host, p, err := bus.HostPort(bus.URL())
		if err != nil {
			t.Fatalf("HostPort(URL()): %v", err)
		}
		ns, err := startEmbeddedNATS(host, p, token)
		if err != nil {
			lastErr = err // port likely taken in the pick-bind window; retry
			continue
		}
		defer func() {
			ns.Shutdown()
			ns.WaitForShutdown()
		}()

		b, err := bus.Connect(bus.URL(), token)
		if err != nil {
			t.Fatalf("Connect(bus.URL()) against the override-bound server: %v", err)
		}
		b.Close()
		return
	}
	t.Fatalf("embedded NATS failed to bind a fresh free port after 3 attempts: %v", lastErr)
}
