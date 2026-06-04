package focus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
)

// RealSystem wires Focus to the live host: /proc for process inspection,
// hyprctl for window control, and tmux for pane control.
func RealSystem() System {
	return System{
		Ancestors:   ancestors,
		Environ:     environ,
		Clients:     hyprlandClients,
		FocusWindow: hyprlandFocus,
		Tmux:        runTmux,
	}
}

// ancestors returns pid followed by its parent chain, stopping at init. It
// reuses liveness.ParentPID so there is a single /proc/<pid>/stat parser.
// maxAncestorWalk caps the parent-chain walk so a cycle or pathological tree
// can't loop forever; deeper than this and we give up locating the window.
const maxAncestorWalk = 32

func ancestors(pid int) ([]int, error) {
	chain := []int{pid}
	for range maxAncestorWalk {
		ppid, err := liveness.ParentPID(pid)
		if err != nil {
			return nil, err
		}
		if ppid <= 1 {
			break
		}
		chain = append(chain, ppid)
		pid = ppid
	}
	return chain, nil
}

func environ(pid int) (map[string]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	env := make(map[string]string)
	for kv := range strings.SplitSeq(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

func hyprlandClients() ([]Client, error) {
	out, err := exec.Command("hyprctl", "clients", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("hyprctl clients: %w", err)
	}
	var clients []Client
	if err := json.Unmarshal(out, &clients); err != nil {
		return nil, fmt.Errorf("parse hyprctl clients: %w", err)
	}
	return clients, nil
}

func hyprlandFocus(address string) error {
	// hyprctl wraps the argument as `return hl.dispatch(<arg>)`, so it must be a
	// dispatcher call. hl.dsp.focus follows the window to its workspace.
	dispatch := fmt.Sprintf("hl.dsp.focus({window='address:%s'})", address)
	if err := exec.Command("hyprctl", "dispatch", dispatch).Run(); err != nil {
		return fmt.Errorf("hyprctl focus %s: %w", address, err)
	}
	return nil
}

func runTmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
