// Package hook receives lifecycle callbacks from agent tools (Claude Code,
// opencode, ...) and republishes them onto the bus as session events.
//
// Errors are logged to stderr but never propagated to the caller — a failing
// hook must not break the user's actual agent session.
package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/bus"
	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
	"github.com/just-barcodes/agentic-sessions-manager/internal/store"
)

// attachIdentity fingerprints the agent process that launched this hook so the
// daemon can later tell whether the session is still alive, and stamps the
// hostname so sessions from remote machines keep their own host_id (the
// daemon's /proc reaper must not probe pids that live on another machine).
// Best-effort: if no durable ancestor is found the session is simply left
// un-probeable.
func attachIdentity(e *session.Event) {
	if id, ok := liveness.Capture(); ok {
		e.PID, e.PIDStart, e.BootID = id.PID, id.Start, id.BootID
	}
	if host, err := os.Hostname(); err == nil {
		e.HostID = host
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
	SessionTitle     string `json:"session_title"`     // SessionStart/UserPromptSubmit only: Claude's AI-generated session title
	TranscriptPath   string `json:"transcript_path"`   // path to the session's JSONL transcript
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
	if title := claudeTitle(in, kind); title != "" {
		e.Payload["title"] = title
	}
	e.Notify = claudeNotify(in.NotificationType, in.Message)
	return e, true, nil
}

// titleTailBytes is how much of a transcript's tail claudeTitle scans for the
// newest ai-title record. Claude rewrites the record every few turns, so the
// last one sits near the end — in a 10 MB transcript, within the final 32 KB.
// A title that falls outside the window simply isn't reported, and the one
// already stored stands.
const titleTailBytes = 64 << 10

// claudeTitle resolves the session's title. Claude puts session_title on
// SessionStart and UserPromptSubmit only, and even there it lags: the title
// generated during a turn isn't visible until the *next* prompt, so a session
// that gets one prompt and then sits idle would never report one. For the events
// that end a turn or block on the user — exactly the sessions `sm ls` exists to
// surface — fall back to the transcript, where Claude writes the title as soon
// as it exists. PreToolUse is deliberately excluded: it is the high-frequency
// hook and it blocks the agent, so it must not pay for a file read.
func claudeTitle(in claudeInput, kind session.EventKind) string {
	if in.SessionTitle != "" {
		return in.SessionTitle
	}
	switch kind {
	case session.EventStop, session.EventSessionEnd, session.EventNotification:
		return titleFromTranscript(in.TranscriptPath)
	}
	return ""
}

// titleFromTranscript returns the newest title recorded in the transcript at
// path, or "" if the scanned tail holds none. Every failure is silent: a missing
// title must never break a hook.
//
// Claude records the title as its own transcript line,
// {"type":"ai-title","aiTitle":"…","sessionId":"…"}, rewritten as the session
// grows. The read starts mid-file, so the first line is usually a fragment; it
// just fails to parse as a record and is skipped.
func titleFromTranscript(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	if off := fi.Size() - titleTailBytes; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return ""
		}
	}
	// Transcript lines (tool results, pasted files) can be far longer than the
	// scanner's default limit, and hitting it would stop the scan early and lose
	// a later title. Nothing in the window can exceed the window itself.
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, titleTailBytes+1)
	var title string
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"ai-title"`)) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			AITitle string `json:"aiTitle"`
		}
		if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "ai-title" {
			continue
		}
		title = rec.AITitle
	}
	return title
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
	token, err := bus.LoadToken(store.BusTokenPath())
	if err != nil {
		return fmt.Errorf("bus token: %w", err)
	}
	b, err := bus.Connect(bus.URL(), token)
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
			Title     string `json:"title"`
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
	payload := map[string]any{}
	if info := in.Properties.Info; info != nil {
		if info.Directory != "" {
			payload["cwd"] = info.Directory
		}
		if info.Title != "" {
			payload["title"] = info.Title
		}
	}
	if len(payload) > 0 {
		e.Payload = payload
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
	// Synthesised by the sm plugin when a session's title changes after the
	// start it already announced. opencode reports that as another
	// session.updated, which would replay as a session start and knock a
	// running turn back to idle; title carries the name and no state.
	case "session.title":
		return session.EventTitle, true
	case "permission.asked":
		return session.EventNotification, true
	case "session.idle":
		return session.EventStop, true
	case "session.error":
		return session.EventFail, true
	}
	return "", false
}
