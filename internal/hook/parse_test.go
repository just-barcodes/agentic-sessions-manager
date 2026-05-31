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

func TestParseClaudeIgnoredEvent(t *testing.T) {
	e, ok, err := parseClaude(strings.NewReader(`{"hook_event_name":"PreCompact"}`), fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for unhandled event, got %+v", e)
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
