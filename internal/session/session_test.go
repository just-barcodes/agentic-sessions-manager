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

// TestTransitionNotification locks the notification_type → state mapping that
// keeps a session from getting stuck in waiting: answering a question resumes
// the agent (→ running), an idle ping must not knock an active turn back to
// waiting, and an absent/unknown type falls back to waiting.
func TestTransitionNotification(t *testing.T) {
	notif := func(typ string) Event {
		e := Event{Kind: EventNotification}
		if typ != "" {
			e.Payload = map[string]any{"notification_type": typ}
		}
		return e
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
		{"permission request waits", StateRunning, notif(NotifyPermission), StateWaiting},
		{"shown question waits", StateRunning, notif(NotifyElicitDialog), StateWaiting},
		{"idle ping while idle waits", StateIdle, notif(NotifyIdle), StateWaiting},
		{"idle ping while running is ignored", StateRunning, notif(NotifyIdle), ""},
		{"absent type falls back to waiting", StateIdle, notif(""), StateWaiting},
		{"unknown type falls back to waiting", StateRunning, notif("future_kind"), StateWaiting},
		{"non-notification uses NextState", StateWaiting, Event{Kind: EventToolUse}, StateRunning},
	}
	for _, c := range cases {
		if got := Transition(c.cur, c.e); got != c.want {
			t.Errorf("%s: Transition(%q, %q) = %q, want %q", c.name, c.cur, notifyType(c.e), got, c.want)
		}
	}
}
