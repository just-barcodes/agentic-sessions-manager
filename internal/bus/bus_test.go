package bus

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

// runTestNATS starts an embedded NATS server on a random free port and returns
// its client URL, shutting it down when the test ends.
func runTestNATS(t *testing.T) string {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("test nats server not ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	return ns.ClientURL()
}

// TestTokenAuth verifies the security boundary token auth provides: a server
// requiring a token rejects unauthenticated and wrong-token clients, accepts
// the right token, and an in-process connection authenticates the same way.
func TestTokenAuth(t *testing.T) {
	const token = "secret"
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, Authorization: token, NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("test nats server not ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})

	if _, err := Connect(ns.ClientURL(), ""); err == nil {
		t.Error("Connect with no token succeeded, want authorization error")
	}
	if _, err := Connect(ns.ClientURL(), "wrong"); err == nil {
		t.Error("Connect with wrong token succeeded, want authorization error")
	}
	b, err := Connect(ns.ClientURL(), token)
	if err != nil {
		t.Fatalf("Connect with correct token: %v", err)
	}
	b.Close()

	ib, err := ConnectInProcess(ns, token)
	if err != nil {
		t.Fatalf("ConnectInProcess with correct token: %v", err)
	}
	ib.Close()
}

// TestEnsureToken covers the token file lifecycle: first call creates an
// owner-only file, repeat calls and LoadToken return the same value, and a
// missing file loads as "" without error.
func TestEnsureToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "bus-token")

	tok, err := EnsureToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Errorf("token length = %d, want 64 hex chars", len(tok))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, want 0600", fi.Mode().Perm())
	}

	again, err := EnsureToken(path)
	if err != nil || again != tok {
		t.Errorf("second EnsureToken = (%q, %v), want same token", again, err)
	}
	loaded, err := LoadToken(path)
	if err != nil || loaded != tok {
		t.Errorf("LoadToken = (%q, %v), want same token", loaded, err)
	}

	missing, err := LoadToken(filepath.Join(t.TempDir(), "nope"))
	if err != nil || missing != "" {
		t.Errorf("LoadToken on missing file = (%q, %v), want (\"\", nil)", missing, err)
	}
}

// TestSessionKey covers the subject routing for first-contact events: an empty
// session id maps to the "new" token so the daemon's wildcard subscription still
// receives events before a UUID is assigned.
func TestSessionKey(t *testing.T) {
	if got := sessionKey(""); got != "new" {
		t.Errorf("sessionKey(\"\") = %q, want %q", got, "new")
	}
	if got := sessionKey("abc"); got != "abc" {
		t.Errorf("sessionKey(\"abc\") = %q, want %q", got, "abc")
	}
}

// TestPublishSubscribeRoundTrip publishes an event with no session id (routed on
// the "new" subject) and verifies the wildcard subscriber receives it intact.
func TestPublishSubscribeRoundTrip(t *testing.T) {
	b, err := Connect(runTestNATS(t), "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	got := make(chan session.Event, 1)
	if err := b.Subscribe(func(e session.Event) { got <- e }); err != nil {
		t.Fatal(err)
	}

	want := session.Event{Agent: "claude", NativeID: "n1", Kind: session.EventNotification, Notify: session.NotifyPermission}
	if err := b.Publish(want); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-got:
		if e.Agent != want.Agent || e.NativeID != want.NativeID || e.Kind != want.Kind || e.Notify != want.Notify {
			t.Errorf("round-trip event = %+v, want %+v", e, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive published event within timeout")
	}
}

// TestDrainWaitsForInflightHandler verifies Drain's core guarantee: it does not
// return until an event handler that is mid-execution has finished. The daemon
// relies on this to ensure no handler is still touching the store when it closes
// it during shutdown.
func TestDrainWaitsForInflightHandler(t *testing.T) {
	b, err := Connect(runTestNATS(t), "")
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	var done atomic.Bool
	if err := b.Subscribe(func(session.Event) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		done.Store(true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(session.Event{Kind: session.EventNote}); err != nil {
		t.Fatal(err)
	}

	<-started // the handler is now executing
	if err := b.Drain(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if !done.Load() {
		t.Error("Drain returned before the in-flight handler completed")
	}
}
