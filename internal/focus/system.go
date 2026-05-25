package focus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// ancestors returns pid followed by its parent chain, stopping at init. The
// /proc/<pid>/stat parsing mirrors internal/liveness; kept local to avoid
// widening that package's API for a second consumer.
func ancestors(pid int) ([]int, error) {
	chain := []int{pid}
	for range 32 {
		ppid, err := parentPID(pid)
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

func parentPID(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// Fields 1 (pid) and 2 (comm) are dropped: comm may contain spaces or
	// parentheses, so split everything after the final ')'. Field 4 (ppid) is
	// then the second field of the remainder.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	return strconv.Atoi(f[1])
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
	var raw []struct {
		Address string `json:"address"`
		PID     int    `json:"pid"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse hyprctl clients: %w", err)
	}
	clients := make([]Client, len(raw))
	for i, r := range raw {
		clients[i] = Client{Address: r.Address, PID: r.PID}
	}
	return clients, nil
}

func hyprlandFocus(address string) error {
	if err := exec.Command("hyprctl", "dispatch", "focuswindow", "address:"+address).Run(); err != nil {
		return fmt.Errorf("hyprctl focuswindow %s: %w", address, err)
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
