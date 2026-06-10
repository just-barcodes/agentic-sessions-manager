// Package cli implements the user-facing subcommands. It reads from the
// store directly (for queries) and publishes to the bus (for `emit`).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/bus"
	"github.com/just-barcodes/agentic-sessions-manager/internal/focus"
	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
	"github.com/just-barcodes/agentic-sessions-manager/internal/store"
)

// reapStale marks sessions whose agent process has died (no clean stop ever
// arrived) on this host. Best-effort: errors are swallowed so a reaping failure
// never blocks listing.
func reapStale(ctx context.Context, st *store.Store) {
	host, err := os.Hostname()
	if err != nil {
		return
	}
	_, _ = st.ReapStale(ctx, host, liveness.IsProcessDead)
}

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

	ctx := context.Background()
	reapStale(ctx, st)

	sessions, err := st.ListSessions(ctx, all)
	if err != nil {
		return err
	}
	const layout = "2006-01-02 15:04"
	fmt.Printf("%-8s  %-10s  %-9s  %-16s  %-16s  %s\n",
		"ID", "AGENT", "STATUS", "STARTED", "LAST", "CWD")
	for _, s := range sessions {
		fmt.Printf("%-8s  %-10s  %-9s  %-16s  %-16s  %s\n",
			short(s.ID), s.Agent, s.Status,
			s.StartedAt.Format(layout), s.LastEventAt.Format(layout), s.CWD)
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

func Mark(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: sm mark <id> <state>")
	}
	state, ok := session.ParseState(args[1])
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
	changed, err := st.UpdateStatus(ctx, id, state, now)
	if err != nil {
		return err
	}
	if changed == 0 {
		// The recency guard skipped the write: a newer event already advanced
		// this session past `now`. Don't claim success or append a note that
		// would misreport the state.
		return fmt.Errorf("mark skipped: %s already has a newer event; status unchanged", short(id))
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

// Focus raises the terminal window (and tmux pane) hosting a session's agent
// process, so the user can jump to a session that is waiting for input.
func Focus(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: sm focus <id>")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	sess, _, err := st.GetSession(context.Background(), args[0], 0)
	if err != nil {
		return err
	}
	return focus.Focus(focus.RealSystem(), liveness.Identity{
		PID:    sess.PID,
		Start:  sess.PIDStart,
		BootID: sess.BootID,
	})
}

func Status(_ []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	reapStale(ctx, st)
	sessions, err := st.ListSessions(ctx, true)
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

	token, err := bus.LoadToken(store.BusTokenPath())
	if err != nil {
		return err
	}
	b, err := bus.Connect(bus.URL(), token)
	if err != nil {
		return err
	}
	defer b.Close()
	return b.Publish(e)
}

func openStore() (*store.Store, error) {
	return store.Open(store.DefaultDBPath())
}

// shortIDLen is how many leading hex chars of a session UUID `sm` displays;
// enough to stay unambiguous in practice while fitting the list column.
const shortIDLen = 8

func short(id string) string {
	if len(id) < shortIDLen {
		return id
	}
	return id[:shortIDLen]
}
