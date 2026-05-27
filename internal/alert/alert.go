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

type Sink interface {
	OnStateChange(sess session.Session) error
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
	return os.WriteFile(c.Path, fmt.Appendf(nil, "%d\n", n), 0o644)
}
