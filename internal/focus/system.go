package focus

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
)

// RealSystem wires Focus to the live host: /proc for process inspection,
// hyprctl for window control, tmux for pane control, and orca-ide for Orca
// tab control, plus Orca's runtime socket for what orca-ide does not expose.
func RealSystem() System {
	return System{
		Ancestors:   ancestors,
		Environ:     environ,
		Clients:     hyprlandClients,
		FocusWindow: hyprlandFocus,
		Tmux:        runTmux,
		Orca:        runOrca,
		OrcaRPC:     orcaRPC,
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

func runOrca(args ...string) (string, error) {
	out, err := exec.Command("orca-ide", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// orcaRPCTimeout bounds one Orca runtime call; keepalive frames do not extend it.
const orcaRPCTimeout = 10 * time.Second

// orcaRPC makes one request to the Orca runtime the way orca-ide does: read
// the runtime metadata (socket path and auth token) from the user-data dir,
// write one JSON line, and read lines until the reply, skipping keepalives.
func orcaRPC(userDataPath, method string, params any) ([]byte, error) {
	if userDataPath == "" {
		return nil, errors.New("agent environment has no ORCA_USER_DATA_PATH")
	}
	b, err := os.ReadFile(filepath.Join(userDataPath, "orca-runtime.json"))
	if err != nil {
		return nil, fmt.Errorf("read orca runtime metadata (is Orca running?): %w", err)
	}
	var meta struct {
		AuthToken  string `json:"authToken"`
		Transports []struct {
			Kind     string `json:"kind"`
			Endpoint string `json:"endpoint"`
		} `json:"transports"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, fmt.Errorf("parse orca runtime metadata: %w", err)
	}
	endpoint := ""
	for _, t := range meta.Transports {
		if t.Kind == "unix" {
			endpoint = t.Endpoint
			break
		}
	}
	if endpoint == "" || meta.AuthToken == "" {
		return nil, errors.New("orca runtime metadata has no unix socket or auth token")
	}

	conn, err := net.DialTimeout("unix", endpoint, orcaRPCTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect to orca runtime: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(orcaRPCTimeout))

	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	id := hex.EncodeToString(idBytes)
	req, err := json.Marshal(map[string]any{"id": id, "authToken": meta.AuthToken, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("write orca runtime request: %w", err)
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var frame struct {
			ID        string          `json:"id"`
			OK        bool            `json:"ok"`
			Keepalive bool            `json:"_keepalive"`
			Result    json.RawMessage `json:"result"`
			Error     struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
			return nil, fmt.Errorf("parse orca runtime reply: %w", err)
		}
		if frame.Keepalive {
			continue
		}
		if frame.ID != id {
			return nil, errors.New("orca runtime replied with a mismatched request id")
		}
		if !frame.OK {
			return nil, fmt.Errorf("orca runtime: %s: %s", frame.Error.Code, frame.Error.Message)
		}
		return frame.Result, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read orca runtime reply: %w", err)
	}
	return nil, errors.New("orca runtime closed the connection without replying")
}
