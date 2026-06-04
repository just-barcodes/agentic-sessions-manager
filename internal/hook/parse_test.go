package hook

import (
	"strings"
	"testing"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

func fixedNow() time.Time { return time.Unix(1700000000, 0) }

func TestParseClaudeEvent(t *testing.T) {
	in := `{"session_id":"abc","hook_event_name":"UserPromptSubmit","cwd":"/tmp/x","prompt":"hello"}`
	e, ok, err := parseClaude(strings.NewReader(in), fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if e.Agent != "claude" {
		t.Errorf("agent = %q, want claude", e.Agent)
	}
	if e.NativeID != "abc" {
		t.Errorf("native id = %q, want abc", e.NativeID)
	}
	if e.Kind != session.EventUserPrompt {
		t.Errorf("kind = %q, want %q", e.Kind, session.EventUserPrompt)
	}
	if !e.Timestamp.Equal(fixedNow()) {
		t.Errorf("timestamp = %v, want %v", e.Timestamp, fixedNow())
	}
	if e.Payload["cwd"] != "/tmp/x" {
		t.Errorf("cwd payload = %v, want /tmp/x", e.Payload["cwd"])
	}
	if e.Payload["prompt"] != "hello" {
		t.Errorf("prompt payload = %v, want hello", e.Payload["prompt"])
	}
}

// TestParseClaudeNotificationToTransition chains the producer (parseClaude) to
// the consumer (session.Transition) to prove the core domain fix end to end: a
// Notification carrying notification_type=elicitation_response must resume the
// agent (→ running), not leave it stuck in waiting.
func TestParseClaudeNotificationToTransition(t *testing.T) {
	in := `{"session_id":"abc","hook_event_name":"Notification","notification_type":"elicitation_response"}`
	e, ok, err := parseClaude(strings.NewReader(in), fixedNow)
	if err != nil || !ok {
		t.Fatalf("parseClaude: ok=%v err=%v", ok, err)
	}
	if e.Notify != session.NotifyElicitResp {
		t.Fatalf("Notify = %q, want %q", e.Notify, session.NotifyElicitResp)
	}
	if got := session.Transition(session.StateWaiting, e); got != session.StateRunning {
		t.Errorf("answering a question left state %q, want running", got)
	}
}

func TestParseClaudeIgnoredEvent(t *testing.T) {
	e, ok, err := parseClaude(strings.NewReader(`{"hook_event_name":"PreCompact"}`), fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for unhandled event, got %+v", e)
	}
}

// TestParseClaudeCompactSkipped verifies a SessionStart fired by compaction
// (source=compact) is dropped: compaction happens mid-session and must not reset
// an active turn to idle. A normal startup SessionStart still flows through.
func TestParseClaudeCompactSkipped(t *testing.T) {
	compact := `{"session_id":"abc","hook_event_name":"SessionStart","source":"compact"}`
	if _, ok, err := parseClaude(strings.NewReader(compact), fixedNow); ok || err != nil {
		t.Fatalf("compact SessionStart: ok=%v err=%v, want ok=false", ok, err)
	}
	startup := `{"session_id":"abc","hook_event_name":"SessionStart","source":"startup"}`
	e, ok, err := parseClaude(strings.NewReader(startup), fixedNow)
	if err != nil || !ok {
		t.Fatalf("startup SessionStart: ok=%v err=%v", ok, err)
	}
	if e.Kind != session.EventSessionStart {
		t.Errorf("kind = %q, want %q", e.Kind, session.EventSessionStart)
	}
}

func TestParseClaudeBadJSON(t *testing.T) {
	if _, _, err := parseClaude(strings.NewReader("{not json"), fixedNow); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestParseOpencodeEvent(t *testing.T) {
	in := `{"type":"permission.asked","properties":{"sessionID":"sess-1","info":{"directory":"/work"}}}`
	e, ok, err := parseOpencode(strings.NewReader(in), fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if e.Agent != "opencode" {
		t.Errorf("agent = %q, want opencode", e.Agent)
	}
	if e.NativeID != "sess-1" {
		t.Errorf("native id = %q, want sess-1", e.NativeID)
	}
	if e.Kind != session.EventNotification {
		t.Errorf("kind = %q, want %q", e.Kind, session.EventNotification)
	}
	if e.Payload["cwd"] != "/work" {
		t.Errorf("cwd payload = %v, want /work", e.Payload["cwd"])
	}
}

// TestParseOpencodeSessionCreated covers the session.created/updated path: it
// maps to a session_start and lifts properties.info.directory into the cwd
// payload so a new session records where it is running.
func TestParseOpencodeSessionCreated(t *testing.T) {
	for _, typ := range []string{"session.created", "session.updated"} {
		in := `{"type":"` + typ + `","properties":{"sessionID":"sess-2","info":{"directory":"/proj"}}}`
		e, ok, err := parseOpencode(strings.NewReader(in), fixedNow)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", typ, ok, err)
		}
		if e.Kind != session.EventSessionStart {
			t.Errorf("%s: kind = %q, want %q", typ, e.Kind, session.EventSessionStart)
		}
		if e.NativeID != "sess-2" {
			t.Errorf("%s: native id = %q, want sess-2", typ, e.NativeID)
		}
		if e.Payload["cwd"] != "/proj" {
			t.Errorf("%s: cwd payload = %v, want /proj", typ, e.Payload["cwd"])
		}
	}
}

func TestParseOpencodeNoSessionID(t *testing.T) {
	e, ok, err := parseOpencode(strings.NewReader(`{"type":"session.idle","properties":{"sessionID":""}}`), fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when session id is empty, got %+v", e)
	}
}

func TestOpencodeKindMapping(t *testing.T) {
	cases := map[string]struct {
		want session.EventKind
		ok   bool
	}{
		"session.created":  {session.EventSessionStart, true},
		"session.updated":  {session.EventSessionStart, true},
		"permission.asked": {session.EventNotification, true},
		"session.idle":     {session.EventStop, true},
		"session.error":    {session.EventFail, true},
		"unknown.event":    {"", false},
	}
	for typ, want := range cases {
		got, ok := opencodeKind(typ)
		if ok != want.ok || got != want.want {
			t.Errorf("opencodeKind(%q) = (%q, %v), want (%q, %v)", typ, got, ok, want.want, want.ok)
		}
	}
}
