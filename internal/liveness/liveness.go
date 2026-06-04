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
	"sync"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
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

const (
	// maxShellWalk caps how many ancestors Capture climbs while skipping shells
	// before giving up — guards against a pathological process tree.
	maxShellWalk = 16

	// /proc/<pid>/stat field indices, numbered after statFields drops fields 1-2
	// so f[0] is field 3. ppid is field 4; start time is field 22.
	statIdxPPID  = 1  // f[1] = field 4
	statIdxStart = 19 // f[19] = field 22
)

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
	for range maxShellWalk {
		ppid, err := ParentPID(pid)
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

// IsProcessDead reports whether the process fingerprinted on s is gone — the
// negation of Alive over s's fingerprint fields. Centralised so the reaper
// predicates in the daemon and CLI share one field mapping and cannot drift.
func IsProcessDead(s session.Session) bool {
	return !Alive(Identity{PID: s.PID, Start: s.PIDStart, BootID: s.BootID})
}

// bootID is invariant per boot, so read /proc once and reuse the result across
// every liveness probe rather than re-reading on each sweep.
var bootID = sync.OnceValues(func() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
})

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

// ParentPID returns pid's parent pid from /proc/<pid>/stat. Exported so the
// focus package can reuse the single /proc stat parser rather than duplicating it.
func ParentPID(pid int) (int, error) {
	f, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) <= statIdxPPID {
		return 0, os.ErrInvalid
	}
	return strconv.Atoi(f[statIdxPPID])
}

func startTime(pid int) (uint64, error) {
	f, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) <= statIdxStart {
		return 0, os.ErrInvalid
	}
	return strconv.ParseUint(f[statIdxStart], 10, 64)
}
