package focus

import (
	"errors"
	"strings"
	"testing"

	"github.com/just-barcodes/agentic-sessions-manager/internal/liveness"
)

// recorder captures the side effects a System would perform so tests can assert
// what was focused and which tmux commands ran.
type recorder struct {
	focused  string
	tmuxRuns [][]string
	orcaRuns [][]string
}

// fakeTmux returns a Tmux func that records every invocation and replies via
// resp, keyed on the first argument (the tmux subcommand).
func fakeTmux(rec *recorder, resp map[string]string) func(...string) (string, error) {
	return func(args ...string) (string, error) {
		rec.tmuxRuns = append(rec.tmuxRuns, args)
		if len(args) == 0 {
			return "", nil
		}
		return resp[args[0]], nil
	}
}

func TestFocusBareRaisesOwningWindow(t *testing.T) {
	rec := &recorder{}
	sys := System{
		// agent (4242) → shell (4200) → terminal (1614, a window owner)
		Ancestors: func(int) ([]int, error) { return []int{4242, 4200, 1614}, nil },
		Clients: func() ([]Client, error) {
			return []Client{{Address: "0xAAA", PID: 9999}, {Address: "0xBBB", PID: 1614}}, nil
		},
		FocusWindow: func(addr string) error { rec.focused = addr; return nil },
	}
	if err := focusBare(sys, 4242); err != nil {
		t.Fatalf("focusBare: %v", err)
	}
	if rec.focused != "0xBBB" {
		t.Errorf("focused %q, want 0xBBB", rec.focused)
	}
}

func TestFocusBareNoOwningWindow(t *testing.T) {
	sys := System{
		Ancestors:   func(int) ([]int, error) { return []int{4242, 4200}, nil },
		Clients:     func() ([]Client, error) { return []Client{{Address: "0xAAA", PID: 1614}}, nil },
		FocusWindow: func(string) error { t.Fatal("should not focus when no window owns the tree"); return nil },
	}
	if err := focusBare(sys, 4242); err == nil {
		t.Fatal("expected error when no ancestor owns a window")
	}
}

func TestFocusTmuxPrefersClientOnSession(t *testing.T) {
	rec := &recorder{}
	resp := map[string]string{
		"display-message": "work\n",
		// a client (pid 5500) is already viewing session "work"
		"list-clients": "5500\t/dev/pts/3\t1700000000\n",
	}
	sys := System{
		Ancestors:   func(int) ([]int, error) { return []int{5500, 5400, 9510}, nil },
		Clients:     func() ([]Client, error) { return []Client{{Address: "0xCAFE", PID: 9510}}, nil },
		FocusWindow: func(addr string) error { rec.focused = addr; return nil },
		Tmux:        fakeTmux(rec, resp),
	}
	if err := focusTmux(sys, "%7"); err != nil {
		t.Fatalf("focusTmux: %v", err)
	}
	if rec.focused != "0xCAFE" {
		t.Errorf("focused %q, want 0xCAFE", rec.focused)
	}
	if got := tmuxSubcommands(rec); got != "display-message,list-clients,select-window,select-pane" {
		t.Errorf("tmux calls = %s; expected no switch-client when a client already views the session", got)
	}
	assertPaneTargeted(t, rec, "%7")
}

func TestFocusTmuxSwitchesClientWhenSessionDetached(t *testing.T) {
	rec := &recorder{}
	calls := 0
	sys := System{
		Ancestors:   func(int) ([]int, error) { return []int{6600, 9510}, nil },
		Clients:     func() ([]Client, error) { return []Client{{Address: "0xFEED", PID: 9510}}, nil },
		FocusWindow: func(addr string) error { rec.focused = addr; return nil },
		Tmux: func(args ...string) (string, error) {
			rec.tmuxRuns = append(rec.tmuxRuns, args)
			switch args[0] {
			case "display-message":
				return "work\n", nil
			case "list-clients":
				calls++
				if calls == 1 {
					return "", nil // none on session "work"
				}
				return "6600\t/dev/pts/9\t1700000500\n", nil // fallback: a client elsewhere
			}
			return "", nil
		},
	}
	if err := focusTmux(sys, "%2"); err != nil {
		t.Fatalf("focusTmux: %v", err)
	}
	if rec.focused != "0xFEED" {
		t.Errorf("focused %q, want 0xFEED", rec.focused)
	}
	if got := tmuxSubcommands(rec); !strings.Contains(got, "switch-client") {
		t.Errorf("tmux calls = %s; expected switch-client when session was detached", got)
	}
}

func TestFocusTmuxNoClientAnywhere(t *testing.T) {
	rec := &recorder{}
	resp := map[string]string{"display-message": "work\n", "list-clients": "\n"}
	sys := System{
		Tmux:        fakeTmux(rec, resp),
		FocusWindow: func(string) error { t.Fatal("should not focus when no client is attached"); return nil },
	}
	if err := focusTmux(sys, "%1"); err == nil {
		t.Fatal("expected error when no tmux client is attached anywhere")
	}
}

const orcaList = `{"ok":true,"result":{"terminals":[
	{"handle":"term_aaa","tabId":"tab-nvim","title":"NVIM"},
	{"handle":"term_bbb","tabId":"tab-claude","title":"Claude Code","agentIdentity":"claude"}]}}`

const orcaSwitched = `{"ok":true,"result":{"focus":{"handle":"term_bbb","tabId":"tab-claude","navigated":true}}}`

// fakeOrca returns an Orca func that records every invocation and replies via
// resp, keyed on the orca-ide subcommand (the second argument).
func fakeOrca(rec *recorder, resp map[string]string) func(...string) (string, error) {
	return func(args ...string) (string, error) {
		rec.orcaRuns = append(rec.orcaRuns, args)
		if len(args) < 2 {
			return "", nil
		}
		return resp[args[1]], nil
	}
}

func orcaClients() ([]Client, error) {
	return []Client{
		{Address: "0xKITTY", PID: 1614, Class: "kitty"},
		{Address: "0x0RCA", PID: 222383, Class: "orca"},
	}, nil
}

func TestFocusOrcaSwitchesTabThenRaisesWindow(t *testing.T) {
	rec := &recorder{}
	var order []string
	sys := System{
		Orca: func(args ...string) (string, error) {
			rec.orcaRuns = append(rec.orcaRuns, args)
			order = append(order, "orca:"+args[1])
			switch args[1] {
			case "list":
				return orcaList, nil
			case "switch":
				return orcaSwitched, nil
			}
			return "", nil
		},
		Clients:     orcaClients,
		FocusWindow: func(addr string) error { rec.focused = addr; order = append(order, "focus"); return nil },
		Ancestors:   func(int) ([]int, error) { t.Fatal("orca path must not walk ancestors"); return nil, nil },
	}
	if err := focusOrca(sys, "tab-claude"); err != nil {
		t.Fatalf("focusOrca: %v", err)
	}
	if rec.focused != "0x0RCA" {
		t.Errorf("focused %q, want the orca window 0x0RCA", rec.focused)
	}
	if got := strings.Join(order, ","); got != "orca:list,orca:switch,focus" {
		t.Errorf("order = %s; want tab switch before window raise", got)
	}
	// The handle passed to switch is the one resolved from the list by tab id.
	sw := rec.orcaRuns[1]
	if want := []string{"terminal", "switch", "--terminal", "term_bbb", "--json"}; strings.Join(sw, " ") != strings.Join(want, " ") {
		t.Errorf("switch args = %v, want %v", sw, want)
	}
}

func TestFocusOrcaTabNotFound(t *testing.T) {
	rec := &recorder{}
	sys := System{
		Orca:        fakeOrca(rec, map[string]string{"list": orcaList}),
		Clients:     orcaClients,
		FocusWindow: func(string) error { t.Fatal("should not focus when the tab is unknown"); return nil },
	}
	err := focusOrca(sys, "tab-gone")
	if err == nil {
		t.Fatal("expected error when the tab id is not in the terminal list")
	}
	if !strings.Contains(err.Error(), "tab-gone") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q should name the missing tab", err)
	}
	if len(rec.orcaRuns) != 1 {
		t.Errorf("orca calls = %v; expected only the list, no switch", rec.orcaRuns)
	}
}

func TestFocusOrcaStartsAppWhenNotRunning(t *testing.T) {
	orcaStartDelay = 0
	rec := &recorder{}
	lists, clientCalls := 0, 0
	sys := System{
		Orca: func(args ...string) (string, error) {
			rec.orcaRuns = append(rec.orcaRuns, args)
			switch args[1] {
			case "list":
				lists++
				if lists <= 2 { // down before open, then briefly not ready after it
					return "", errors.New("runtime_unavailable")
				}
				return orcaList, nil
			case "switch":
				return orcaSwitched, nil
			}
			return "", nil
		},
		Clients: func() ([]Client, error) {
			clientCalls++
			if clientCalls == 1 { // window not mapped yet right after launch
				return nil, nil
			}
			return orcaClients()
		},
		FocusWindow: func(addr string) error { rec.focused = addr; return nil },
	}
	if err := focusOrca(sys, "tab-claude"); err != nil {
		t.Fatalf("focusOrca: %v", err)
	}
	if rec.focused != "0x0RCA" {
		t.Errorf("focused %q, want the orca window", rec.focused)
	}
	subs := make([]string, len(rec.orcaRuns))
	for i, c := range rec.orcaRuns {
		subs[i] = strings.Join(c[:2], " ")
	}
	want := "terminal list,open --json,terminal list,terminal list,terminal switch"
	if got := strings.Join(subs, ","); got != want {
		t.Errorf("orca calls = %s; want %s", got, want)
	}
}

func TestFocusOrcaCannotStartApp(t *testing.T) {
	sys := System{
		Orca:        func(...string) (string, error) { return "", errors.New("exit status 1") },
		Clients:     orcaClients,
		FocusWindow: func(string) error { t.Fatal("should not focus when Orca cannot start"); return nil },
	}
	err := focusOrca(sys, "tab-claude")
	if err == nil {
		t.Fatal("expected error when orca-ide open fails")
	}
	if !strings.Contains(err.Error(), "could not be started") {
		t.Errorf("error %q should say Orca could not be started", err)
	}
}

func TestFocusOrcaStillUnreachableAfterStart(t *testing.T) {
	orcaStartDelay = 0
	sys := System{
		Orca: func(args ...string) (string, error) {
			if args[0] == "open" {
				return "", nil
			}
			return "", errors.New("runtime_unavailable")
		},
		FocusWindow: func(string) error { t.Fatal("should not focus when the runtime never comes up"); return nil },
	}
	err := focusOrca(sys, "tab-claude")
	if err == nil {
		t.Fatal("expected error when the terminal list stays unreachable after open")
	}
	if !strings.Contains(err.Error(), "after starting Orca") {
		t.Errorf("error %q should say the list failed after starting Orca", err)
	}
}

func TestFocusOrcaNoOrcaWindow(t *testing.T) {
	rec := &recorder{}
	sys := System{
		Orca:        fakeOrca(rec, map[string]string{"list": orcaList, "switch": orcaSwitched}),
		Clients:     func() ([]Client, error) { return []Client{{Address: "0xKITTY", PID: 1614, Class: "kitty"}}, nil },
		FocusWindow: func(string) error { t.Fatal("should not focus a non-orca window"); return nil },
	}
	err := focusOrca(sys, "tab-claude")
	if err == nil {
		t.Fatal("expected error when no window has class orca")
	}
	if !strings.Contains(err.Error(), `class "orca"`) {
		t.Errorf("error %q should name the missing window class", err)
	}
}

func TestFocusRejectsUnfingerprintedSession(t *testing.T) {
	err := Focus(System{}, liveness.Identity{PID: 0})
	if err == nil {
		t.Fatal("expected error for a session with no captured pid")
	}
}

func TestFocusRejectsDeadProcess(t *testing.T) {
	// A fabricated identity that liveness.Alive will reject (bogus boot id).
	dead := liveness.Identity{PID: 1, Start: 1, BootID: "00000000-0000-0000-0000-000000000000"}
	if err := Focus(System{}, dead); err == nil {
		t.Fatal("expected error for a dead process")
	}
}

func TestFocusRoutesByEnviron(t *testing.T) {
	// Capture a real, live identity (the test process's parent) so the alive
	// check passes; then drive routing purely through the injected Environ.
	live, ok := liveness.Capture()
	if !ok {
		t.Skip("could not capture a live identity in this environment")
	}

	t.Run("tmux when TMUX_PANE set", func(t *testing.T) {
		rec := &recorder{}
		routed := false
		sys := System{
			Environ: func(int) (map[string]string, error) { return map[string]string{"TMUX_PANE": "%5"}, nil },
			Tmux: func(args ...string) (string, error) {
				routed = true
				rec.tmuxRuns = append(rec.tmuxRuns, args)
				if args[0] == "display-message" {
					return "s\n", nil
				}
				return "7000\t/dev/pts/1\t1\n", nil
			},
			Ancestors:   func(int) ([]int, error) { return []int{7000}, nil },
			Clients:     func() ([]Client, error) { return []Client{{Address: "0x1", PID: 7000}}, nil },
			FocusWindow: func(string) error { return nil },
		}
		if err := Focus(sys, live); err != nil {
			t.Fatalf("Focus: %v", err)
		}
		if !routed {
			t.Error("expected the tmux path when TMUX_PANE is set")
		}
	})

	t.Run("orca when ORCA_TAB_ID set", func(t *testing.T) {
		rec := &recorder{}
		sys := System{
			Environ:     func(int) (map[string]string, error) { return map[string]string{"ORCA_TAB_ID": "tab-claude"}, nil },
			Orca:        fakeOrca(rec, map[string]string{"list": orcaList, "switch": orcaSwitched}),
			Clients:     orcaClients,
			FocusWindow: func(addr string) error { rec.focused = addr; return nil },
			Ancestors:   func(int) ([]int, error) { t.Fatal("should not walk ancestors on the orca path"); return nil, nil },
			Tmux:        func(...string) (string, error) { t.Fatal("should not call tmux on the orca path"); return "", nil },
		}
		if err := Focus(sys, live); err != nil {
			t.Fatalf("Focus: %v", err)
		}
		if rec.focused != "0x0RCA" {
			t.Errorf("focused %q, want the orca window", rec.focused)
		}
	})

	t.Run("bare when TMUX_PANE absent", func(t *testing.T) {
		focused := false
		sys := System{
			Environ:     func(int) (map[string]string, error) { return map[string]string{}, nil },
			Ancestors:   func(int) ([]int, error) { return []int{live.PID}, nil },
			Clients:     func() ([]Client, error) { return []Client{{Address: "0x9", PID: live.PID}}, nil },
			FocusWindow: func(string) error { focused = true; return nil },
			Tmux:        func(...string) (string, error) { t.Fatal("should not call tmux on the bare path"); return "", nil },
		}
		if err := Focus(sys, live); err != nil {
			t.Fatalf("Focus: %v", err)
		}
		if !focused {
			t.Error("expected the bare path to focus a window")
		}
	})
}

func TestFocusSurfacesClientError(t *testing.T) {
	sys := System{
		Ancestors: func(int) ([]int, error) { return []int{1}, nil },
		Clients:   func() ([]Client, error) { return nil, errors.New("hyprctl down") },
	}
	if err := focusBare(sys, 1); err == nil {
		t.Fatal("expected the hyprctl error to propagate")
	}
}

func tmuxSubcommands(rec *recorder) string {
	subs := make([]string, len(rec.tmuxRuns))
	for i, call := range rec.tmuxRuns {
		subs[i] = call[0]
	}
	return strings.Join(subs, ",")
}

func assertPaneTargeted(t *testing.T, rec *recorder, pane string) {
	t.Helper()
	for _, call := range rec.tmuxRuns {
		if call[0] == "select-pane" {
			if call[len(call)-1] != pane {
				t.Errorf("select-pane targeted %q, want %q", call[len(call)-1], pane)
			}
			return
		}
	}
	t.Error("select-pane was never called")
}
