// Package session defines the core domain types: sessions, states, and events.
// It has no I/O dependencies and is safe to import from any other package.
package session

import (
	"slices"
	"time"
)

type State string

const (
	StateRunning  State = "running"
	StateWaiting  State = "waiting"
	StateIdle     State = "idle"     // alive but between turns; set by Stop. Also set manually via `sm mark`.
	StateFinished State = "finished" // session terminated cleanly; set only by SessionEnd.
	StateFailed   State = "failed"
	StateDead     State = "dead" // agent process gone without a clean stop; set by the reaper.
)

type Session struct {
	ID          string // UUID assigned by the daemon on first event.
	Agent       string // "claude", "opencode", ...
	NativeID    string // The agent's own session identifier, used to correlate hook events.
	CWD         string
	HostID      string // hostname today; reserved for cross-device later.
	StartedAt   time.Time
	LastEventAt time.Time
	Status      State
	PID         int    // OS pid of the agent process; 0 when not captured.
	PIDStart    uint64 // PID's /proc start time, used to detect pid reuse.
	BootID      string // boot id when PID was captured.

	// LastPrompt is the text of the most recent user prompt, derived from
	// events rather than stored on the row. Only ListSessions populates it;
	// other lookups leave it empty.
	LastPrompt string `json:",omitempty"`
}

type EventKind string

const (
	EventSessionStart EventKind = "session_start"
	EventUserPrompt   EventKind = "user_prompt"  // user submitted a prompt; a turn is starting
	EventToolUse      EventKind = "tool_use"     // a tool invocation began; the agent is working
	EventNotification EventKind = "notification" // agent waiting for input or permission
	EventStop         EventKind = "stop"         // end of a response turn
	EventSessionEnd   EventKind = "session_end"  // session terminated (clean exit)
	EventFail         EventKind = "fail"
	EventNote         EventKind = "note" // free-form progress event
)

type Event struct {
	SessionID string         `json:"session_id,omitempty"` // assigned by daemon; empty on first contact
	NativeID  string         `json:"native_id,omitempty"`  // the agent's own session id; used by the daemon to map to SessionID
	Agent     string         `json:"agent,omitempty"`      // "claude", "opencode", ...
	Kind      EventKind      `json:"kind"`
	Timestamp time.Time      `json:"ts"`
	Payload   map[string]any `json:"payload,omitempty"`
	PID       int            `json:"pid,omitempty"`       // agent process id, captured by the hook
	PIDStart  uint64         `json:"pid_start,omitempty"` // PID's /proc start time
	BootID    string         `json:"boot_id,omitempty"`   // boot id when PID was captured
}

// NextState returns the state a session should transition to after observing
// an event of the given kind. The empty string means "no state change".
func NextState(k EventKind) State {
	switch k {
	case EventSessionStart, EventUserPrompt, EventToolUse, EventNote:
		return StateRunning
	case EventNotification:
		return StateWaiting
	case EventStop:
		return StateIdle
	case EventSessionEnd:
		return StateFinished
	case EventFail:
		return StateFailed
	}
	return ""
}

// IsTerminal reports whether s is an end state: a session that has finished
// cleanly or been reaped. Terminal sessions are hidden from the default list
// view and skipped by the reaper.
func IsTerminal(s State) bool {
	return slices.Contains(TerminalStates(), s)
}

// TerminalStates is the single source of truth for which states are terminal.
// The store builds its filters from this so a new terminal state propagates
// everywhere automatically.
func TerminalStates() []State {
	return []State{StateFinished, StateDead}
}

// ParseState validates a user-supplied state string for `sm mark`. Only states
// a user may set manually are accepted; StateDead is reaper-only and rejected.
func ParseState(s string) (State, bool) {
	switch State(s) {
	case StateRunning, StateWaiting, StateIdle, StateFinished, StateFailed:
		return State(s), true
	}
	return "", false
}
