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
	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

// attachIdentity fingerprints the agent process that launched this hook so the
// daemon can later tell whether the session is still alive. Best-effort: if no
// durable ancestor is found the session is simply left un-probeable.
func attachIdentity(e *session.Event) {
	if id, ok := liveness.Capture(); ok {
		e.PID, e.PIDStart, e.BootID = id.PID, id.Start, id.BootID
	}
}

// Run is the `sm hook <agent>` entry point. It always returns nil; any error
// is written to stderr.
//
//	sm hook claude      # reads Claude Code hook JSON from stdin
//	sm hook opencode    # reads opencode plugin event JSON from stdin
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
	case "opencode":
		return runOpencode(os.Stdin)
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
	attachIdentity(&e)

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

// opencodeInput is the subset of an opencode plugin event payload we use.
// Verified against opencode 1.14.46: every session-level event we care about
// carries the id at properties.sessionID, and session.* events optionally
// carry the cwd at properties.info.directory.
type opencodeInput struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		Info      *struct {
			Directory string `json:"directory"`
		} `json:"info,omitempty"`
	} `json:"properties"`
}

func runOpencode(r io.Reader) error {
	var in opencodeInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return fmt.Errorf("decode opencode hook input: %w", err)
	}
	kind, ok := opencodeKind(in.Type)
	if !ok {
		return nil
	}
	if in.Properties.SessionID == "" {
		return nil
	}

	e := session.Event{
		Agent:     "opencode",
		NativeID:  in.Properties.SessionID,
		Kind:      kind,
		Timestamp: time.Now(),
	}
	if in.Properties.Info != nil && in.Properties.Info.Directory != "" {
		e.Payload = map[string]any{"cwd": in.Properties.Info.Directory}
	}
	attachIdentity(&e)

	b, err := bus.Connect(bus.DefaultURL)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer b.Close()
	return b.Publish(e)
}

// opencodeKind maps opencode event type names to session.EventKind.
// Note: opencode 1.14.46 emits "permission.asked" at runtime even though the
// installed @opencode-ai/plugin v1.14.20 types name it "permission.updated".
func opencodeKind(typeStr string) (session.EventKind, bool) {
	switch typeStr {
	case "session.created", "session.updated":
		return session.EventSessionStart, true
	case "permission.asked":
		return session.EventNotification, true
	case "session.idle":
		return session.EventStop, true
	case "session.error":
		return session.EventFail, true
	}
	return "", false
}
