package session

import "testing"

// TestNextState locks the event→state mapping. The two distinctions that matter
// most: Stop ends a turn (→ idle, the agent is alive and waiting at the prompt),
// while only SessionEnd means the session terminated (→ finished).
func TestNextState(t *testing.T) {
	cases := []struct {
		kind EventKind
		want State
	}{
		{EventSessionStart, StateRunning},
		{EventUserPrompt, StateRunning},
		{EventToolUse, StateRunning},
		{EventNote, StateRunning},
		{EventNotification, StateWaiting},
		{EventStop, StateIdle},
		{EventSessionEnd, StateFinished},
		{EventFail, StateFailed},
		{EventKind("unknown"), ""}, // unrecognized kind: no transition
	}
	for _, c := range cases {
		if got := NextState(c.kind); got != c.want {
			t.Errorf("NextState(%q) = %q, want %q", c.kind, got, c.want)
		}
	}
}
