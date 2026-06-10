package bdd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDaemonLifecycle is the harness spike: a per-test daemon starts
// hermetically (temp XDG dirs, non-default port via SM_BUS_URL), proves
// readiness with a token-auth bus connection, and tears down without leaving
// a process behind.
func TestDaemonLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("BDD suite skipped in -short mode")
	}
	w := newWorld(t)
	w.startDaemon(t)

	// Readiness inside startDaemon already proved a token-auth connect; do it
	// once more explicitly so the assertion lives in the test, not the helper.
	if err := w.connectBus(); err != nil {
		t.Fatalf("token-auth connect to per-test daemon: %v\ndaemon stderr:\n%s", err, w.daemonStderr())
	}

	// The token the daemon minted must follow the repo's owner-only convention.
	if fi, err := os.Stat(filepath.Join(w.dataDir, "sm", "bus-token")); err != nil {
		t.Errorf("bus-token file: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("bus-token mode = %04o, want 0600", fi.Mode().Perm())
	}

	w.stopDaemon(t)
	if w.daemonRunning() {
		t.Fatal("daemon process still running after stopDaemon")
	}
}

// TestHookSessionSurvivesReaper pins the liveness assumption the whole BDD
// suite rests on: a session created via `sm hook claude` is fingerprinted
// against the hook's durable ancestor — this test binary — which stays alive
// for the test's duration, so reap passes must not mark the session dead.
// Every `sm ls` runs a reap pass itself, so repeated listing exercises the
// reaper without waiting for the daemon's 30s sweep.
func TestHookSessionSurvivesReaper(t *testing.T) {
	if testing.Short() {
		t.Skip("BDD suite skipped in -short mode")
	}
	w := newWorld(t)
	w.startDaemon(t)

	const start = `{"session_id":"live-1","hook_event_name":"SessionStart","source":"startup","cwd":"/tmp/bdd-liveness"}`
	if out, err := w.sm(strings.NewReader(start), "hook", "claude"); err != nil {
		t.Fatalf("sm hook claude: %v\n%s", err, out)
	}

	// Wait for the event to land (delivery is async), then keep listing: each
	// call reaps, and the session must stay idle rather than turn dead.
	deadline := time.Now().Add(2 * time.Second)
	var out string
	for {
		var err error
		out, err = w.sm(nil, "ls", "--all")
		if err != nil {
			t.Fatalf("sm ls: %v\n%s", err, out)
		}
		if strings.Contains(out, "claude") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "claude") {
		t.Fatalf("session never appeared in sm ls\nlast output:\n%s\ndaemon stderr:\n%s", out, w.daemonStderr())
	}

	for range 3 {
		var err error
		out, err = w.sm(nil, "ls", "--all")
		if err != nil {
			t.Fatalf("sm ls: %v\n%s", err, out)
		}
		if strings.Contains(out, "dead") {
			t.Fatalf("session was reaped despite a live ancestor\noutput:\n%s\ndaemon stderr:\n%s", out, w.daemonStderr())
		}
	}
	if !strings.Contains(out, "idle") {
		t.Fatalf("session not idle after reap passes\noutput:\n%s", out)
	}
}
