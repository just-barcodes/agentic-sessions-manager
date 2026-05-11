// Package hook receives lifecycle callbacks from agent tools (Claude Code,
// opencode, ...) and republishes them onto the bus as session events.
//
// Errors are logged to stderr but never propagated to the caller — a failing
// hook must not break the user's actual agent session.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/bus"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

// Run is the `sm hook <agent>` entry point. It always returns nil; any error
// is written to stderr.
//
//	sm hook claude    # reads Claude Code hook JSON from stdin
func Run(args []string) error {
	if err := dispatch(args); err != nil {
		fmt.Fprintln(os.Stderr, "sm hook:", err)
	}
	return nil
}

func dispatch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sm hook <agent>")
	}
	switch args[0] {
	case "claude":
		return runClaude(os.Stdin)
	default:
		return fmt.Errorf("unknown agent: %q", args[0])
	}
}

// claudeInput is the subset of Claude Code's hook stdin JSON we use. Claude
// fires hooks with at least these fields; ignore everything else.
type claudeInput struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
}

func runClaude(r io.Reader) error {
	var in claudeInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return fmt.Errorf("decode claude hook input: %w", err)
	}
	kind, ok := claudeKind(in.HookEventName)
	if !ok {
		return nil // event we don't care about — succeed silently
	}

	e := session.Event{
		Agent:     "claude",
		NativeID:  in.SessionID,
		Kind:      kind,
		Timestamp: time.Now(),
	}
	if in.CWD != "" {
		e.Payload = map[string]any{"cwd": in.CWD}
	}

	b, err := bus.Connect(bus.DefaultURL)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer b.Close()
	return b.Publish(e)
}

func claudeKind(name string) (session.EventKind, bool) {
	switch name {
	case "SessionStart":
		return session.EventSessionStart, true
	case "Notification":
		return session.EventNotification, true
	case "Stop":
		return session.EventStop, true
	}
	return "", false
}
