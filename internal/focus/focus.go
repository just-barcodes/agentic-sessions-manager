// Package focus locates and raises the terminal window (and tmux pane) that
// hosts a session's agent process, so the user can jump to a session that is
// waiting for input. Hyprland + tmux specific; reads Linux /proc to walk the
// process tree.
//
// The session already carries the agent process fingerprint (pid, start time,
// boot id) captured for liveness, so focus derives the window from that pid at
// jump time rather than storing any window/pane locator. Deriving live also
// self-corrects: a tmux session moved to another window is found where it is now.
package focus

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
)

// Client is the subset of a `hyprctl clients -j` entry that focus needs. The
// pid is the window owner — the terminal emulator, not the agent inside it.
type Client struct {
	Address string `json:"address"`
	PID     int    `json:"pid"`
}

// System is the set of OS/window-manager interactions Focus depends on. It is a
// struct of funcs (not a live Hyprland/tmux) so the resolution logic is unit
// testable; RealSystem wires these to /proc, hyprctl, and tmux.
type System struct {
	Ancestors   func(pid int) ([]int, error) // pid then its ancestors, nearest first
	Environ     func(pid int) (map[string]string, error)
	Clients     func() ([]Client, error)             // hyprctl clients -j
	FocusWindow func(address string) error           // raise window (follows it to its workspace)
	Tmux        func(args ...string) (string, error) // run tmux, return stdout
}

// Focus raises the window hosting the agent process identified by id. It refuses
// sessions whose process was never fingerprinted or has since exited, then
// branches on whether the agent runs inside tmux.
func Focus(sys System, id liveness.Identity) error {
	if id.PID <= 0 {
		return errors.New("session has no process fingerprint; cannot locate its window")
	}
	if !liveness.Alive(id) {
		return errors.New("session's agent process is gone (exited or the host rebooted)")
	}
	env, err := sys.Environ(id.PID)
	if err != nil {
		return fmt.Errorf("read process environment: %w", err)
	}
	if pane := strings.TrimSpace(env["TMUX_PANE"]); pane != "" {
		return focusTmux(sys, pane)
	}
	return focusBare(sys, id.PID)
}

// focusBare handles an agent running directly in a terminal window: the window
// owner is one of the agent's process ancestors.
func focusBare(sys System, pid int) error {
	addr, err := windowAddrFor(sys, pid)
	if err != nil {
		return err
	}
	return sys.FocusWindow(addr)
}

// focusTmux handles an agent running inside a tmux pane. The agent's parent is
// the (reparented) tmux server, so the ancestor walk can't reach a window;
// instead it locates a client viewing the pane's session and raises that
// client's terminal window, then selects the pane within tmux.
func focusTmux(sys System, pane string) error {
	session, err := sys.Tmux("display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return fmt.Errorf("resolve tmux session for pane %s: %w", pane, err)
	}
	session = strings.TrimSpace(session)

	// Prefer a client already viewing the session (non-intrusive: just raise it).
	pid, tty, ok, err := pickClient(sys, "-t", session)
	if err != nil {
		return err
	}
	switchNeeded := false
	if !ok {
		// No client on this session: take the most-recently-active client
		// anywhere and switch it to the session.
		pid, tty, ok, err = pickClient(sys)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("tmux session %q has no attached client; attach a terminal to it first", session)
		}
		switchNeeded = true
	}

	addr, err := windowAddrFor(sys, pid)
	if err != nil {
		return err
	}
	if err := sys.FocusWindow(addr); err != nil {
		return err
	}
	if switchNeeded {
		if _, err := sys.Tmux("switch-client", "-c", tty, "-t", session); err != nil {
			return fmt.Errorf("tmux switch-client: %w", err)
		}
	}
	if _, err := sys.Tmux("select-window", "-t", pane); err != nil {
		return fmt.Errorf("tmux select-window: %w", err)
	}
	if _, err := sys.Tmux("select-pane", "-t", pane); err != nil {
		return fmt.Errorf("tmux select-pane: %w", err)
	}
	return nil
}

// windowAddrFor returns the address of the Hyprland window that owns pid's
// process tree: the nearest ancestor of pid that is itself a window owner.
func windowAddrFor(sys System, pid int) (string, error) {
	chain, err := sys.Ancestors(pid)
	if err != nil {
		return "", fmt.Errorf("walk ancestors of pid %d: %w", pid, err)
	}
	clients, err := sys.Clients()
	if err != nil {
		return "", fmt.Errorf("list hyprland windows: %w", err)
	}
	// Map each owning pid to its first window. A pid owning several windows
	// (single-instance terminals) is inherently ambiguous; first wins.
	addrByPID := make(map[int]string, len(clients))
	for _, c := range clients {
		if _, dup := addrByPID[c.PID]; !dup {
			addrByPID[c.PID] = c.Address
		}
	}
	for _, p := range chain {
		if addr, ok := addrByPID[p]; ok {
			return addr, nil
		}
	}
	return "", fmt.Errorf("no hyprland window owns the process tree (pids %v)", chain)
}

// pickClient returns the most-recently-active tmux client matching the given
// list-clients filter (e.g. "-t", session). ok is false when none match.
func pickClient(sys System, filter ...string) (pid int, tty string, ok bool, err error) {
	args := append([]string{"list-clients", "-F", "#{client_pid}\t#{client_tty}\t#{client_activity}"}, filter...)
	out, err := sys.Tmux(args...)
	if err != nil {
		return 0, "", false, fmt.Errorf("tmux list-clients: %w", err)
	}
	var bestActivity int64 = -1
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		p, convErr := strconv.Atoi(f[0])
		if convErr != nil {
			continue
		}
		activity, _ := strconv.ParseInt(f[2], 10, 64)
		if activity > bestActivity {
			bestActivity, pid, tty, ok = activity, p, f[1], true
		}
	}
	return pid, tty, ok, nil
}
