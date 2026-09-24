// Package focus locates and raises the terminal window (and tmux pane, or
// Orca tab) that hosts a session's agent process, so the user can jump to a
// session that is waiting for input. Hyprland + tmux + Orca specific; reads
// Linux /proc to walk the process tree.
//
// The session already carries the agent process fingerprint (pid, start time,
// boot id) captured for liveness, so focus derives the window from that pid at
// jump time rather than storing any window/pane locator. Deriving live also
// self-corrects: a tmux session moved to another window is found where it is now.
package focus

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
)

// tmuxPaneRe matches a tmux pane id (e.g. "%7"). TMUX_PANE is read from
// /proc/<pid>/environ, so validate it before passing it to tmux's -t flag to
// keep an attacker-controlled value from being interpreted as an option.
var tmuxPaneRe = regexp.MustCompile(`^%[0-9]+$`)

// Client is the subset of a `hyprctl clients -j` entry that focus needs. The
// pid is the window owner — the terminal emulator, not the agent inside it.
type Client struct {
	Address string `json:"address"`
	PID     int    `json:"pid"`
	Class   string `json:"class"`
}

// orcaWindowClass is the Hyprland window class of the Orca IDE.
const orcaWindowClass = "orca"

// After `orca-ide open` returns, the runtime briefly reports its graph as not
// ready and the window may not be mapped yet, so the list and window lookups
// are polled. Tests shorten the delay.
var (
	orcaStartAttempts = 20
	orcaStartDelay    = 500 * time.Millisecond
)

// System is the set of OS/window-manager interactions Focus depends on. It is a
// struct of funcs (not a live Hyprland/tmux) so the resolution logic is unit
// testable; RealSystem wires these to /proc, hyprctl, tmux, and orca-ide.
type System struct {
	Ancestors   func(pid int) ([]int, error) // pid then its ancestors, nearest first
	Environ     func(pid int) (map[string]string, error)
	Clients     func() ([]Client, error)             // hyprctl clients -j
	FocusWindow func(address string) error           // raise window (follows it to its workspace)
	Tmux        func(args ...string) (string, error) // run tmux, return stdout
	Orca        func(args ...string) (string, error) // run orca-ide, return stdout
}

// Focus raises the window hosting the agent process identified by id. It refuses
// sessions whose process was never fingerprinted or has since exited, then
// branches on whether the agent runs inside tmux or an Orca tab.
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
	if tab := strings.TrimSpace(env["ORCA_TAB_ID"]); tab != "" {
		return focusOrca(sys, tab)
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
	if !tmuxPaneRe.MatchString(pane) {
		return fmt.Errorf("refusing malformed tmux pane id %q", pane)
	}
	session, err := sys.Tmux("display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return fmt.Errorf("resolve tmux session for pane %s: %w", pane, err)
	}
	session = strings.TrimSpace(session)

	pid, tty, switchNeeded, err := pickSessionClient(sys, session)
	if err != nil {
		return err
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

// focusOrca handles an agent running in a tab of the Orca IDE. Orca's terminals
// live under a background daemon that owns no window, so the ancestor walk can
// never succeed; instead it asks Orca to switch to the tab, then raises Orca's
// window. The tab id is stable across Orca restarts while the terminal handle
// in the agent's environment is not, so the handle is re-resolved from Orca's
// terminal list each time. The daemon outlives the app, so when the app is not
// running (the terminal list is unreachable) it is launched first.
func focusOrca(sys System, tab string) error {
	launched := false
	out, err := sys.Orca("terminal", "list", "--json")
	if err != nil {
		if _, oerr := sys.Orca("open", "--json"); oerr != nil {
			return fmt.Errorf("orca-ide open (Orca is not running and could not be started): %w", oerr)
		}
		launched = true
		err = orcaPoll(func() error {
			out, err = sys.Orca("terminal", "list", "--json")
			return err
		})
		if err != nil {
			return fmt.Errorf("orca-ide terminal list after starting Orca: %w", err)
		}
	}
	var list struct {
		Result struct {
			Terminals []struct {
				Handle string `json:"handle"`
				TabID  string `json:"tabId"`
			} `json:"terminals"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return fmt.Errorf("parse orca-ide terminal list: %w", err)
	}
	handle := ""
	for _, t := range list.Result.Terminals {
		if t.TabID == tab {
			handle = t.Handle
			break
		}
	}
	if handle == "" {
		return fmt.Errorf("orca tab %s not found in orca-ide terminal list (tab closed?)", tab)
	}

	out, err = sys.Orca("terminal", "switch", "--terminal", handle, "--json")
	if err != nil {
		return fmt.Errorf("orca-ide terminal switch %s: %w", handle, err)
	}
	var sw struct {
		Result struct {
			Focus struct {
				Navigated bool `json:"navigated"`
			} `json:"focus"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &sw); err != nil {
		return fmt.Errorf("parse orca-ide terminal switch: %w", err)
	}
	if !sw.Result.Focus.Navigated {
		return fmt.Errorf("orca-ide did not navigate to tab %s", tab)
	}

	addr := ""
	find := func() error {
		clients, err := sys.Clients()
		if err != nil {
			return fmt.Errorf("list hyprland windows: %w", err)
		}
		for _, c := range clients {
			if c.Class == orcaWindowClass {
				addr = c.Address
				return nil
			}
		}
		return fmt.Errorf("no hyprland window with class %q", orcaWindowClass)
	}
	if launched {
		err = orcaPoll(find)
	} else {
		err = find()
	}
	if err != nil {
		return err
	}
	return sys.FocusWindow(addr)
}

// orcaPoll retries fn until it succeeds or orcaStartAttempts are used up,
// returning the last error.
func orcaPoll(fn func() error) error {
	var err error
	for i := range orcaStartAttempts {
		if i > 0 {
			time.Sleep(orcaStartDelay)
		}
		if err = fn(); err == nil {
			return nil
		}
	}
	return err
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

// pickSessionClient finds the tmux client to drive when focusing a pane in
// session: it prefers a client already viewing the session (just raise it), and
// otherwise falls back to the most-recently-active client anywhere — reporting
// switchNeeded so the caller switches that client to the session first.
func pickSessionClient(sys System, session string) (pid int, tty string, switchNeeded bool, err error) {
	pid, tty, ok, err := pickClient(sys, "-t", session)
	if err != nil {
		return 0, "", false, err
	}
	if ok {
		return pid, tty, false, nil
	}
	pid, tty, ok, err = pickClient(sys)
	if err != nil {
		return 0, "", false, err
	}
	if !ok {
		return 0, "", false, fmt.Errorf("tmux session %q has no attached client; attach a terminal to it first", session)
	}
	return pid, tty, true, nil
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
