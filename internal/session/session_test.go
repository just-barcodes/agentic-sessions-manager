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
		{EventSessionStart, StateIdle},
		{EventUserPrompt, StateRunning},
		{EventToolUse, StateRunning},
		{EventNote, StateRunning},
		{EventNotification, ""}, // notifications are routed by Transition, not NextState
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

func TestIsTerminal(t *testing.T) {
	cases := map[State]bool{
		StateFinished: true,
		StateDead:     true,
		StateRunning:  false,
		StateWaiting:  false,
		StateIdle:     false,
		StateFailed:   false,
	}
	for state, want := range cases {
		if got := IsTerminal(state); got != want {
			t.Errorf("IsTerminal(%q) = %v, want %v", state, got, want)
		}
	}
}

// TestParseState verifies only user-settable states are accepted and that the
// reaper-only StateDead is rejected from `sm mark`.
func TestParseState(t *testing.T) {
	cases := map[string]struct {
		want State
		ok   bool
	}{
		"running":  {StateRunning, true},
		"waiting":  {StateWaiting, true},
		"idle":     {StateIdle, true},
		"finished": {StateFinished, true},
		"failed":   {StateFailed, true},
		"dead":     {"", false}, // reaper-only, not user-settable
		"bogus":    {"", false},
		"":         {"", false},
	}
	for in, want := range cases {
		got, ok := ParseState(in)
		if got != want.want || ok != want.ok {
			t.Errorf("ParseState(%q) = (%q, %v), want (%q, %v)", in, got, ok, want.want, want.ok)
		}
	}
}

// TestTransitionNotification locks the notification_type → state mapping that
// keeps a session from getting stuck in waiting: answering a question resumes
// the agent (→ running), a 60s idle ping never changes state (so a fresh/cleared
// idle session is not flagged as waiting), and an absent/unknown type falls back
// to waiting.
func TestTransitionNotification(t *testing.T) {
	notif := func(typ NotifyType) Event {
		return Event{Kind: EventNotification, Notify: typ}
	}
	cases := []struct {
		name string
		cur  State
		e    Event
		want State
	}{
		{"answer resumes the agent", StateWaiting, notif(NotifyElicitResp), StateRunning},
		{"elicitation complete resumes", StateWaiting, notif(NotifyElicitDone), StateRunning},
		{"auth success resumes", StateWaiting, notif(NotifyAuthSuccess), StateRunning},
		{"auth success on idle session is ignored", StateIdle, notif(NotifyAuthSuccess), ""},
		{"elicitation complete on idle session is ignored", StateIdle, notif(NotifyElicitDone), ""},
		{"auth success on running session is ignored", StateRunning, notif(NotifyAuthSuccess), ""},
		{"permission request waits", StateRunning, notif(NotifyPermission), StateWaiting},
		{"shown question waits", StateRunning, notif(NotifyElicitDialog), StateWaiting},
		{"idle ping while idle stays idle", StateIdle, notif(NotifyIdle), ""},
		{"idle ping while running is ignored", StateRunning, notif(NotifyIdle), ""},
		{"absent type falls back to waiting", StateIdle, notif(""), StateWaiting},
		{"unknown type falls back to waiting", StateRunning, notif("future_kind"), StateWaiting},
		{"non-notification uses NextState", StateWaiting, Event{Kind: EventToolUse}, StateRunning},
	}
	for _, c := range cases {
		if got := Transition(c.cur, c.e); got != c.want {
			t.Errorf("%s: Transition(%q, %q) = %q, want %q", c.name, c.cur, c.e.Notify, got, c.want)
		}
	}
}
