// Package alert delivers state changes to the user. The Sink interface lets
// the daemon fan out to multiple destinations (desktop notifications, walker
// status file, future webhooks, etc.) without coupling them to each other.
package alert

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

type Sink interface {
	OnStateChange(sess session.Session) error
}

// NotifySend pops a desktop notification on attention-worthy transitions.
type NotifySend struct{}

func (NotifySend) OnStateChange(sess session.Session) error {
	summary, ok := summaryFor(sess.Status)
	if !ok {
		return nil
	}
	body := fmt.Sprintf("%s — %s", sess.Agent, sess.CWD)
	return exec.Command("notify-send", "-a", "sm", summary, body).Run()
}

func summaryFor(s session.State) (string, bool) {
	switch s {
	case session.StateWaiting:
		return "Agent waiting for input", true
	case session.StateFinished:
		return "Agent finished", true
	case session.StateFailed:
		return "Agent failed", true
	}
	return "", false
}

// CountFile maintains a small "waiting count" file that walker / status bars
// can read on demand. Count is injected so this sink doesn't depend on store.
type CountFile struct {
	Path  string
	Count func() (int, error)
}

func (c CountFile) OnStateChange(_ session.Session) error {
	n, err := c.Count()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.Path, []byte(fmt.Sprintf("%d\n", n)), 0o644)
}
