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
	Notify    NotifyType     `json:"notify,omitempty"`    // sub-type for Notification events; empty otherwise
	PID       int            `json:"pid,omitempty"`       // agent process id, captured by the hook
	PIDStart  uint64         `json:"pid_start,omitempty"` // PID's /proc start time
	BootID    string         `json:"boot_id,omitempty"`   // boot id when PID was captured
}

// NotifyType is the sub-type of a Notification event, set by the hook from
// Claude's notification_type field. Typing it gives the hook (producer) and
// Transition (consumer) a shared, compile-time-checked contract.
//
// Claude Code's Notification hook fires for several distinct sub-events that do
// not all mean "the agent is blocked": elicitation_complete/response fire when
// the user *answers* a question (the agent is resuming), and auth_success is
// informational. Not every Claude version populates the type
// (anthropics/claude-code#11964), so an absent type falls back to the
// conservative "waiting".
type NotifyType string

const (
	NotifyPermission   NotifyType = "permission_prompt"    // agent needs permission — blocked
	NotifyIdle         NotifyType = "idle_prompt"          // 60s idle, "waiting for your input"
	NotifyAuthSuccess  NotifyType = "auth_success"         // login completed — informational
	NotifyElicitDialog NotifyType = "elicitation_dialog"   // a question is shown — blocked
	NotifyElicitDone   NotifyType = "elicitation_complete" // the question was answered — resuming
	NotifyElicitResp   NotifyType = "elicitation_response" // the question was answered — resuming
)

// NextState returns the state a session should transition to after observing
// an event of the given kind. The empty string means "no state change".
// Notifications are not handled here because their meaning depends on the
// Notify sub-type — see Transition.
//
// session_start maps to idle, not running: a session that has just started,
// resumed, cleared (/clear), or compacted is sitting at the prompt waiting for
// the user — no work is in flight. (Claude fires no Stop after a start, so a
// session_start → running mapping would stick at running until the first turn
// completed.) The active-turn signal comes from user_prompt / tool_use.
func NextState(k EventKind) State {
	switch k {
	case EventUserPrompt, EventToolUse, EventNote:
		return StateRunning
	case EventSessionStart, EventStop:
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

// Transition returns the state a session in state cur should move to after
// observing e. "" means no change. Every event maps purely from its kind
// (NextState) except Notification, whose meaning depends on its sub-type:
//
//   - answering a question (elicitation_complete/response) or completing auth
//     resumes the agent → running, but only from waiting: these signal the end
//     of a block, so they must not start a turn on a session that wasn't blocked.
//     A background auth_success (e.g. a periodic token refresh) on an idle /
//     just-cleared session would otherwise spuriously flip it to running;
//   - a permission request or a shown question → waiting;
//   - a 60s idle ping ("Claude is waiting for your input") is informational and
//     never changes state: the agent isn't blocked on a decision, the user just
//     hasn't typed yet. A freshly started or /cleared session sits at idle and
//     must stay there rather than being flagged as needing attention; an active
//     turn (running) must not be knocked back either;
//   - an absent or unrecognised type → waiting (the safe default that preserves
//     "agent asked → waiting" on Claude versions that omit notification_type).
func Transition(cur State, e Event) State {
	if e.Kind != EventNotification {
		return NextState(e.Kind)
	}
	switch e.Notify {
	case NotifyElicitDone, NotifyElicitResp, NotifyAuthSuccess:
		if cur == StateWaiting {
			return StateRunning
		}
		return ""
	case NotifyPermission, NotifyElicitDialog:
		return StateWaiting
	case NotifyIdle:
		return ""
	default:
		return StateWaiting
	}
}
