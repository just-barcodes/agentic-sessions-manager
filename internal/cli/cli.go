// Package cli implements the user-facing subcommands. It reads from the
// store directly (for queries) and publishes to the bus (for `emit`).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/bus"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
	"github.com/just-barcodes/agentic-sessions-manager/internal/store"
)

func List(args []string) error {
	all := false
	for _, a := range args {
		if a == "--all" || a == "-a" {
			all = true
		}
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	sessions, err := st.ListSessions(context.Background(), all)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		fmt.Printf("%-8s  %-10s  %-9s  %s\n",
			short(s.ID), s.Agent, s.Status, s.CWD)
	}
	return nil
}

func Show(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: sm show <id>")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	sess, events, err := st.GetSession(context.Background(), args[0], 50)
	if err != nil {
		return err
	}

	fmt.Printf("ID:         %s\n", sess.ID)
	fmt.Printf("Agent:      %s\n", sess.Agent)
	if sess.NativeID != "" {
		fmt.Printf("Native ID:  %s\n", sess.NativeID)
	}
	fmt.Printf("CWD:        %s\n", sess.CWD)
	fmt.Printf("Status:     %s\n", sess.Status)
	fmt.Printf("Host:       %s\n", sess.HostID)
	fmt.Printf("Started:    %s\n", sess.StartedAt.Format(time.RFC3339))
	fmt.Printf("Last event: %s\n", sess.LastEventAt.Format(time.RFC3339))

	if len(events) == 0 {
		fmt.Println("\n(no events)")
		return nil
	}
	fmt.Printf("\nEvents (%d, oldest first):\n", len(events))
	for _, e := range events {
		fmt.Printf("  %s  %-14s  %s\n",
			e.Timestamp.Format("15:04:05"),
			e.Kind,
			payloadInline(e.Payload))
	}
	return nil
}

func payloadInline(p map[string]any) string {
	if len(p) == 0 {
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func Watch(_ []string) error {
	// TODO: subscribe to bus.SubjectEventAll and print each event as JSON.
	return errors.New("not implemented")
}

func Mark(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: sm mark <id> <state>")
	}
	state, ok := parseState(args[1])
	if !ok {
		return fmt.Errorf("invalid state %q (want one of: running, waiting, idle, finished, failed)", args[1])
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	id, err := st.ResolveSessionID(ctx, args[0])
	if err != nil {
		return err
	}

	now := time.Now()
	if err := st.UpdateStatus(ctx, id, state, now); err != nil {
		return err
	}
	if err := st.AppendEvent(ctx, session.Event{
		SessionID: id,
		Kind:      session.EventNote,
		Timestamp: now,
		Payload:   map[string]any{"manual_state": string(state)},
	}); err != nil {
		return err
	}

	fmt.Printf("%s → %s\n", short(id), state)
	return nil
}

func parseState(s string) (session.State, bool) {
	st := session.State(s)
	switch st {
	case session.StateRunning, session.StateWaiting, session.StateIdle,
		session.StateFinished, session.StateFailed:
		return st, true
	}
	return "", false
}

func Status(_ []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	sessions, err := st.ListSessions(context.Background(), true)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(sessions)
}

// Emit publishes a single event to the bus. Used by hook scripts.
//
// Reserved keys mapped onto Event top-level fields:
//
//	session=<uuid>       — daemon-assigned id (rarely set by hooks)
//	agent=<name>         — "claude", "opencode", ...
//	native=<id>          — the agent's own session id; the daemon uses this
//	                       plus agent to correlate subsequent events.
//
// All other key=value args become payload entries.
//
//	sm emit session_start agent=claude native=$CLAUDE_SESSION_ID cwd=$PWD
//	sm emit notification  agent=claude native=$CLAUDE_SESSION_ID
//	sm emit stop          agent=claude native=$CLAUDE_SESSION_ID
func Emit(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: sm emit <kind> [key=value ...]")
	}
	e := session.Event{
		Kind:      session.EventKind(args[0]),
		Timestamp: time.Now(),
		Payload:   map[string]any{},
	}
	for _, kv := range args[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("bad arg %q (want key=value)", kv)
		}
		switch k {
		case "session":
			e.SessionID = v
		case "agent":
			e.Agent = v
		case "native":
			e.NativeID = v
		default:
			e.Payload[k] = v
		}
	}

	b, err := bus.Connect(bus.DefaultURL)
	if err != nil {
		return err
	}
	defer b.Close()
	return b.Publish(e)
}

func openStore() (*store.Store, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local/share")
	}
	return store.Open(filepath.Join(dataDir, "sm", "sm.db"))
}

func short(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

