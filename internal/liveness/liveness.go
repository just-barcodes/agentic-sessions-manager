// Package liveness fingerprints and probes OS processes so the manager can tell
// whether the agent behind a session is still running, rather than guessing from
// how long it has been silent. Linux-only: it reads /proc.
//
// A process is identified by the tuple (pid, start time, boot id). Start time
// defeats pid reuse — a recycled pid has a different start time — and boot id
// invalidates every captured pid across a reboot.
package liveness

import (
	"os"
	"strconv"
	"strings"
)

// Identity is a process fingerprint that stays stable for the life of a process
// within a single boot. A zero PID means "not captured".
type Identity struct {
	PID    int
	Start  uint64 // /proc/<pid>/stat field 22 (start time, clock ticks since boot)
	BootID string
}

var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true,
	"fish": true, "ksh": true, "ash": true, "csh": true, "tcsh": true,
}

// Capture fingerprints the agent process that launched the current hook by
// walking up the process tree, skipping any intervening shell (Claude runs
// hooks via `sh -c`). It returns ok=false when no durable ancestor can be found
// — e.g. the hook was reparented to init — leaving the session un-probeable.
func Capture() (Identity, bool) {
	boot, err := bootID()
	if err != nil {
		return Identity{}, false
	}
	pid := os.Getpid()
	for range 16 {
		ppid, err := parentPID(pid)
		if err != nil || ppid <= 1 {
			return Identity{}, false
		}
		if shells[comm(ppid)] {
			pid = ppid
			continue
		}
		start, err := startTime(ppid)
		if err != nil {
			return Identity{}, false
		}
		return Identity{PID: ppid, Start: start, BootID: boot}, true
	}
	return Identity{}, false
}

// Alive reports whether the fingerprinted process is still running. A changed
// boot id (reboot), a missing pid, or a mismatched start time (pid reused) all
// mean the original process is gone.
func Alive(id Identity) bool {
	if id.PID <= 0 || id.BootID == "" {
		return false
	}
	if boot, err := bootID(); err != nil || boot != id.BootID {
		return false
	}
	start, err := startTime(id.PID)
	if err != nil {
		return false
	}
	return start == id.Start
}

func bootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func comm(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// statFields returns /proc/<pid>/stat split from field 3 onward. Fields 1 (pid)
// and 2 (comm) are dropped because comm may contain spaces or parentheses;
// everything after the final ')' splits safely. So f[0] is field 3, f[1] is
// field 4 (ppid), and field N is f[N-3].
func statFields(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil, os.ErrInvalid
	}
	return strings.Fields(s[i+1:]), nil
}

func parentPID(pid int) (int, error) {
	f, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) < 2 {
		return 0, os.ErrInvalid
	}
	return strconv.Atoi(f[1]) // field 4
}

func startTime(pid int) (uint64, error) {
	f, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) < 20 {
		return 0, os.ErrInvalid
	}
	return strconv.ParseUint(f[19], 10, 64) // field 22
}
