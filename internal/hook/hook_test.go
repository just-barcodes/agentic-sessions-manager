package hook

import (
	"testing"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

func TestClaudeKindSessionEnd(t *testing.T) {
	kind, ok := claudeKind("SessionEnd")
	if !ok {
		t.Fatal("SessionEnd hook not recognized")
	}
	if kind != session.EventSessionEnd {
		t.Fatalf("SessionEnd mapped to %q, want %q", kind, session.EventSessionEnd)
	}
	if got := session.NextState(kind); got != session.StateFinished {
		t.Errorf("SessionEnd transitions to %q, want %q", got, session.StateFinished)
	}
}

// TestClaudeNotify checks the notification sub-type resolution: an explicit
// notification_type wins, and when it is absent the message text is classified so
// a 60s idle reminder lands the session at idle (no change) rather than waiting.
func TestClaudeNotify(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		msg  string
		want session.NotifyType
		// raw Transition output from an idle session; "" means "stays idle".
		wantNext session.State
	}{
		{"explicit type wins", "permission_prompt", "anything", session.NotifyPermission, session.StateWaiting},
		{"permission from message", "", "Claude needs your permission", session.NotifyPermission, session.StateWaiting},
		{"idle reminder from message", "", "Claude is waiting for your input", session.NotifyIdle, ""},
		{"unknown message falls back to waiting", "", "something else", session.NotifyType(""), session.StateWaiting},
		{"empty message falls back to waiting", "", "", session.NotifyType(""), session.StateWaiting},
	}
	for _, c := range cases {
		got := claudeNotify(c.typ, c.msg)
		if got != c.want {
			t.Errorf("%s: claudeNotify(%q, %q) = %q, want %q", c.name, c.typ, c.msg, got, c.want)
		}
		e := session.Event{Kind: session.EventNotification, Notify: got}
		if next := session.Transition(session.StateIdle, e); next != c.wantNext {
			t.Errorf("%s: idle + %q → %q, want %q", c.name, got, next, c.wantNext)
		}
	}
}

// TestClaudeKindStates checks each wired Claude hook maps to the kind that lands
// the session in the right state — in particular that Stop is a turn boundary
// (→ idle), not a session end, and that the working signals are recognized.
func TestClaudeKindStates(t *testing.T) {
	cases := []struct {
		hook string
		kind session.EventKind
		want session.State
	}{
		{"SessionStart", session.EventSessionStart, session.StateIdle},
		{"UserPromptSubmit", session.EventUserPrompt, session.StateRunning},
		{"PreToolUse", session.EventToolUse, session.StateRunning},
		{"Notification", session.EventNotification, session.StateWaiting},
		{"Stop", session.EventStop, session.StateIdle},
		{"SessionEnd", session.EventSessionEnd, session.StateFinished},
	}
	for _, c := range cases {
		kind, ok := claudeKind(c.hook)
		if !ok {
			t.Errorf("%s hook not recognized", c.hook)
			continue
		}
		if kind != c.kind {
			t.Errorf("%s mapped to %q, want %q", c.hook, kind, c.kind)
		}
		// Notification's target state depends on notification_type, so it is
		// owned by Transition, not NextState (which leaves it unchanged).
		if got := session.Transition(session.StateIdle, session.Event{Kind: kind}); got != c.want {
			t.Errorf("%s transitions to %q, want %q", c.hook, got, c.want)
		}
	}
}
