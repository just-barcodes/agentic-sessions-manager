// Package alert delivers state changes to the user. The Sink interface lets
// the daemon fan out to multiple destinations (walker status file, future
// webhooks, etc.) without coupling them to each other.
package alert

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

// Sink receives a notification that the session with the given id moved to
// newState. That is the whole signal a state change carries; sinks that need
// more (cwd, agent, ...) should read the store by id.
type Sink interface {
	OnStateChange(id string, newState session.State) error
}

// CountFile maintains a small "waiting count" file that walker / status bars
// can read on demand. Count is injected so this sink doesn't depend on store.
type CountFile struct {
	Path  string
	Count func() (int, error)
}

func (c CountFile) OnStateChange(_ string, _ session.State) error {
	n, err := c.Count()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.Path, fmt.Appendf(nil, "%d\n", n), 0o600)
}
