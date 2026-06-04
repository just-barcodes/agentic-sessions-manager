package bus

import (
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
	b, err := Connect(runTestNATS(t))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	got := make(chan session.Event, 1)
	if err := b.Subscribe(t.Context(), func(e session.Event) { got <- e }); err != nil {
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
