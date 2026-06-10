// Package bdd hosts the black-box BDD suite: every test drives the real sm
// binary end to end (hook JSON on stdin → NATS → daemon → SQLite → sm ls),
// hermetically isolated from the user's live daemon and data. This file is
// lifecycle only — binary build, per-test dirs/port/env, daemon start/stop;
// fixtures and assertions live elsewhere.
package bdd

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/bus"
)

const (
	readyTimeout = 5 * time.Second // daemon must accept a token-auth connect within this
	stopTimeout  = 5 * time.Second // SIGTERM grace before SIGKILL
	startRetries = 3               // free-port picks have a TOCTOU window; retry with a new port
)

// smBin is the sm binary built once by TestMain for the whole suite.
var smBin string

func TestMain(m *testing.M) {
	flag.Parse()
	var dir string
	if !testing.Short() {
		var err error
		dir, err = os.MkdirTemp("", "sm-bdd-*")
		if err != nil {
			log.Fatalf("bdd: temp dir: %v", err)
		}
		smBin = filepath.Join(dir, "sm")
		out, err := exec.Command("go", "build", "-o", smBin,
			"github.com/just-barcodes/agentic-sessions-manager/cmd/sm").CombinedOutput()
		if err != nil {
			os.RemoveAll(dir)
			log.Fatalf("bdd: failed to build sm binary:\n%s", out)
		}
	}
	code := m.Run() // os.Exit skips defers, so clean up before exiting
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

// world is one test's hermetic environment: its own XDG dirs (so its own
// database, bus token, and waiting-count file) and its own bus port. Every sm
// subprocess the test launches shares this env, so the daemon and its clients
// agree on all paths and the token.
type world struct {
	dataDir  string // per-test XDG_DATA_HOME
	stateDir string // per-test XDG_STATE_HOME
	busURL   string

	daemon *exec.Cmd
	exited chan struct{} // closed when the daemon process is reaped

	mu     sync.Mutex
	stderr bytes.Buffer // daemon stderr, attached to failure messages
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		dataDir:  t.TempDir(),
		stateDir: t.TempDir(),
	}
	t.Cleanup(func() { w.stopDaemon(t) })
	return w
}

// env is the environment for every sm subprocess in this world. Overridden
// keys are stripped from the inherited environment rather than appended after
// it, so isolation never depends on duplicate-key resolution order; HOME is
// redirected too so even non-XDG fallback paths stay inside the sandbox.
func (w *world) env() []string {
	overrides := map[string]string{
		"XDG_DATA_HOME":  w.dataDir,
		"XDG_STATE_HOME": w.stateDir,
		"SM_BUS_URL":     w.busURL,
		"HOME":           w.dataDir,
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if _, overridden := overrides[key]; !overridden {
			env = append(env, kv)
		}
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// startDaemon launches `sm daemon` on a fresh free port and blocks until a
// token-auth bus connection succeeds. A daemon that dies before becoming
// ready (e.g. the picked port was taken meanwhile) is retried on a new port.
func (w *world) startDaemon(t *testing.T) {
	t.Helper()
	var lastErr error
	for range startRetries {
		w.busURL = fmt.Sprintf("nats://127.0.0.1:%d", pickFreePort(t))
		if lastErr = w.tryStartDaemon(); lastErr == nil {
			return
		}
	}
	t.Fatalf("daemon not ready after %d attempts: %v\ndaemon stderr:\n%s",
		startRetries, lastErr, w.daemonStderr())
}

func (w *world) tryStartDaemon() error {
	cmd := exec.Command(smBin, "daemon")
	cmd.Env = w.env()
	w.mu.Lock()
	w.stderr.Reset()
	w.mu.Unlock()
	cmd.Stderr = syncWriter{w}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.daemon = cmd
	w.exited = make(chan struct{})
	go func(done chan struct{}) {
		_ = cmd.Wait()
		close(done)
	}(w.exited)

	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-w.exited:
			return fmt.Errorf("daemon exited before becoming ready (port %s in use?)", w.busURL)
		default:
		}
		if err := w.connectBus(); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.killDaemon()
	return fmt.Errorf("daemon not ready within %s on %s", readyTimeout, w.busURL)
}

// connectBus proves end-to-end readiness: the token file the daemon wrote is
// readable from this world's data dir and authenticates a bus connection.
func (w *world) connectBus() error {
	token, err := bus.LoadToken(filepath.Join(w.dataDir, "sm", "bus-token"))
	if err != nil || token == "" {
		return fmt.Errorf("bus token not available yet: %v", err)
	}
	b, err := bus.Connect(w.busURL, token)
	if err != nil {
		return err
	}
	b.Close()
	return nil
}

// sm runs an sm subcommand inside this world's environment, feeding it stdin
// when non-nil and returning its combined output.
func (w *world) sm(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(smBin, args...)
	cmd.Env = w.env()
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stopDaemon terminates the daemon gracefully and waits for it to be reaped:
// SIGTERM, then SIGKILL if it ignores the grace period. Safe to call twice.
func (w *world) stopDaemon(t *testing.T) {
	t.Helper()
	if w.daemon == nil || !w.daemonRunning() {
		return
	}
	_ = w.daemon.Process.Signal(syscall.SIGTERM)
	select {
	case <-w.exited:
	case <-time.After(stopTimeout):
		t.Errorf("daemon ignored SIGTERM for %s; killing\ndaemon stderr:\n%s", stopTimeout, w.daemonStderr())
		w.killDaemon()
	}
}

func (w *world) killDaemon() {
	_ = w.daemon.Process.Kill()
	select {
	case <-w.exited:
	case <-time.After(stopTimeout):
		// cmd.Wait can stall if the process left descendants holding the
		// pipes; give up rather than hang the whole test binary.
	}
}

func (w *world) daemonRunning() bool {
	if w.daemon == nil {
		return false
	}
	select {
	case <-w.exited:
		return false
	default:
		return true
	}
}

func (w *world) daemonStderr() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stderr.String()
}

// syncWriter serializes daemon stderr writes with daemonStderr reads.
type syncWriter struct{ w *world }

func (s syncWriter) Write(p []byte) (int, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	return s.w.stderr.Write(p)
}

// pickFreePort finds a port that was free a moment ago. The pick-then-bind
// window is racy by nature; startDaemon absorbs it by retrying.
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
