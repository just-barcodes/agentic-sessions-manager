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
	"strings"
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
	SessionID        string `json:"session_id"`
	HookEventName    string `json:"hook_event_name"`
	CWD              string `json:"cwd"`
	Source           string `json:"source"`            // SessionStart: startup|resume|clear|compact
	Reason           string `json:"reason"`            // SessionEnd: clear|logout|prompt_input_exit|other
	Prompt           string `json:"prompt"`            // UserPromptSubmit: the submitted prompt text
	Message          string `json:"message"`           // Notification: the human-readable message
	NotificationType string `json:"notification_type"` // Notification: permission_prompt|idle_prompt|elicitation_*|auth_success
}

func runClaude(r io.Reader) error {
	e, ok, err := parseClaude(r, time.Now)
	if err != nil || !ok {
		return err
	}
	attachIdentity(&e)
	return publish(e)
}

// parseClaude decodes Claude Code hook JSON into a session event. ok is false
// for events we don't care about. now is injected so timestamps are testable.
func parseClaude(r io.Reader, now func() time.Time) (session.Event, bool, error) {
	var in claudeInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return session.Event{}, false, fmt.Errorf("decode claude hook input: %w", err)
	}
	kind, ok := claudeKind(in.HookEventName)
	if !ok {
		return session.Event{}, false, nil
	}
	// Compaction (auto or /compact) fires a SessionStart with source=compact
	// after the "recap" is generated. This happens mid-session — often during an
	// active turn — so it must not reset state to idle. Skip it; the real start /
	// resume / clear sources still flow through to idle.
	if kind == session.EventSessionStart && in.Source == "compact" {
		return session.Event{}, false, nil
	}
	e := session.Event{
		Agent:     "claude",
		NativeID:  in.SessionID,
		Kind:      kind,
		Timestamp: now(),
		Payload:   map[string]any{},
	}
	if in.CWD != "" {
		e.Payload["cwd"] = in.CWD
	}
	if in.Reason != "" {
		e.Payload["reason"] = in.Reason
	}
	if in.Prompt != "" {
		e.Payload["prompt"] = in.Prompt
	}
	if in.Message != "" {
		e.Payload["message"] = in.Message
	}
	e.Notify = claudeNotify(in.NotificationType, in.Message)
	return e, true, nil
}

// claudeNotify resolves a Notification's sub-type. Newer Claude versions populate
// notification_type; when it's absent (anthropics/claude-code#11964) we classify
// the human-readable message instead. This is what lets sm distinguish a 60s idle
// reminder ("Claude is waiting for your input") — which must not flag a fresh or
// /cleared session as waiting — from a real permission block. An unrecognised or
// empty message yields "", which Transition treats as the conservative "waiting".
func claudeNotify(typ, msg string) session.NotifyType {
	if typ != "" {
		return session.NotifyType(typ)
	}
	switch {
	case strings.Contains(msg, "needs your permission"):
		return session.NotifyPermission
	case strings.Contains(msg, "waiting for your input"):
		return session.NotifyIdle
	}
	return ""
}

// publish connects to the bus, emits e, and closes the connection.
func publish(e session.Event) error {
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
	case "UserPromptSubmit":
		return session.EventUserPrompt, true
	case "PreToolUse":
		return session.EventToolUse, true
	case "Notification":
		return session.EventNotification, true
	case "Stop":
		return session.EventStop, true
	case "SessionEnd":
		return session.EventSessionEnd, true
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
	e, ok, err := parseOpencode(r, time.Now)
	if err != nil || !ok {
		return err
	}
	attachIdentity(&e)
	return publish(e)
}

// parseOpencode decodes an opencode plugin event into a session event. ok is
// false for events we ignore or that lack a session id. now is injected so
// timestamps are testable.
func parseOpencode(r io.Reader, now func() time.Time) (session.Event, bool, error) {
	var in opencodeInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return session.Event{}, false, fmt.Errorf("decode opencode hook input: %w", err)
	}
	kind, ok := opencodeKind(in.Type)
	if !ok {
		return session.Event{}, false, nil
	}
	if in.Properties.SessionID == "" {
		return session.Event{}, false, nil
	}
	e := session.Event{
		Agent:     "opencode",
		NativeID:  in.Properties.SessionID,
		Kind:      kind,
		Timestamp: now(),
	}
	if in.Properties.Info != nil && in.Properties.Info.Directory != "" {
		e.Payload = map[string]any{"cwd": in.Properties.Info.Directory}
	}
	return e, true, nil
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
