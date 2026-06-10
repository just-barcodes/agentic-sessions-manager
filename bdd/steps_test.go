// Step definitions for the BDD suite. Each scenario gets a fresh scenario
// struct (and with it a fresh world); steps are thin wrappers over the world
// and the fixtures so the behavioral wiring stays readable.
package bdd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

const (
	assertTimeout = 2 * time.Second        // event delivery is async; poll list/count assertions up to this long
	assertEvery   = 20 * time.Millisecond  // polling interval for eventually
	settleTime    = 300 * time.Millisecond // grace before asserting something did NOT happen
)

// scenario is the per-scenario state: the hermetic world plus what the
// scenario has observed so far.
type scenario struct {
	w        *world
	projects map[string]string // project name in the feature → temp dir used as cwd
	rows     []lsRow           // session list as of the last list assertion
}

func initializeScenario(sc *godog.ScenarioContext) {
	s := &scenario{projects: map[string]string{}}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		w, err := newScenarioWorld()
		s.w = w
		return ctx, err
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, scErr error) (context.Context, error) {
		if s.w == nil { // Before failed before the world existed
			return ctx, nil
		}
		if scErr != nil {
			fmt.Fprintf(os.Stderr, "daemon stderr for failed scenario:\n%s\n", s.w.daemonStderr())
		}
		return ctx, s.w.dispose()
	})

	sc.Given(`^the session manager is running$`, s.sessionManagerRunning)
	sc.Step(`^a claude session with native id "([^"]*)" starts in "([^"]*)"$`, s.claudeSessionStarts)
	sc.Step(`^the session list contains exactly (\d+) sessions?$`, s.sessionListContainsExactly)
	sc.Step(`^that session is a "([^"]*)" session in "([^"]*)"$`, s.thatSessionIs)
	sc.Step(`^that session's state is "([^"]*)"$`, s.thatSessionStateIs)
}

func (s *scenario) sessionManagerRunning() error {
	return s.w.start()
}

func (s *scenario) claudeSessionStarts(nativeID, project string) error {
	dir, err := s.projectDir(project)
	if err != nil {
		return err
	}
	return s.runHook("claude", claudeSessionStartJSON(nativeID, dir, "startup"))
}

func (s *scenario) sessionListContainsExactly(n int) error {
	return s.eventually(func() error {
		rows, err := s.listSessions()
		if err != nil {
			return err
		}
		if len(rows) != n {
			return fmt.Errorf("session list has %d sessions, want %d: %+v", len(rows), n, rows)
		}
		s.rows = rows
		return nil
	})
}

func (s *scenario) thatSessionIs(agent, project string) error {
	row, err := s.theSession()
	if err != nil {
		return err
	}
	if row.agent != agent || row.cwd != s.projects[project] {
		return fmt.Errorf("session is agent %q in %q, want %q in %q (%s)",
			row.agent, row.cwd, agent, s.projects[project], project)
	}
	return nil
}

func (s *scenario) thatSessionStateIs(state string) error {
	row, err := s.theSession()
	if err != nil {
		return err
	}
	if row.status != state {
		return fmt.Errorf("session state is %q, want %q", row.status, state)
	}
	return nil
}

// theSession is "that session" in the feature text: the single row the last
// list assertion observed.
func (s *scenario) theSession() (lsRow, error) {
	if len(s.rows) != 1 {
		return lsRow{}, fmt.Errorf(`"that session" needs exactly 1 listed session, have %d`, len(s.rows))
	}
	return s.rows[0], nil
}

// projectDir maps a project name from the feature text to a per-scenario
// directory that serves as the session's cwd.
func (s *scenario) projectDir(name string) (string, error) {
	if dir, ok := s.projects[name]; ok {
		return dir, nil
	}
	dir := filepath.Join(s.w.dataDir, "projects", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	s.projects[name] = dir
	return dir, nil
}

// runHook pipes hook JSON into `sm hook <agent>`. The hook contract is that
// it never breaks the agent, so any non-zero exit is a hard step failure.
func (s *scenario) runHook(agent, input string) error {
	out, err := s.w.sm(strings.NewReader(input), "hook", agent)
	if err != nil {
		return fmt.Errorf("sm hook %s: %v\n%s", agent, err, out)
	}
	return nil
}

// eventually polls check until it succeeds or the assertion deadline passes;
// the last failure is returned with the daemon's stderr attached.
func (s *scenario) eventually(check func() error) error {
	deadline := time.Now().Add(assertTimeout)
	for {
		err := check()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w\ndaemon stderr:\n%s", err, s.w.daemonStderr())
		}
		time.Sleep(assertEvery)
	}
}

// lsRow is one data row of `sm ls`: ID AGENT STATUS STARTED LAST CWD, where
// STARTED and LAST are "date time" pairs.
type lsRow struct {
	id, agent, status, cwd string
}

func (s *scenario) listSessions() ([]lsRow, error) {
	out, err := s.w.sm(nil, "ls")
	if err != nil {
		return nil, fmt.Errorf("sm ls: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var rows []lsRow
	for _, line := range lines[1:] { // skip the header
		f := strings.Fields(line)
		if len(f) < 8 {
			return nil, fmt.Errorf("unparsable sm ls row %q in:\n%s", line, out)
		}
		rows = append(rows, lsRow{id: f[0], agent: f[1], status: f[2], cwd: f[7]})
	}
	return rows, nil
}

// claudeSessionStartJSON is the subset of Claude Code's SessionStart hook
// stdin that sm consumes. source is startup|resume|clear|compact.
func claudeSessionStartJSON(nativeID, cwd, source string) string {
	return fmt.Sprintf(`{"session_id":%q,"hook_event_name":"SessionStart","source":%q,"cwd":%q}`,
		nativeID, source, cwd)
}
