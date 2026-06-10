// Step definitions for the BDD suite. Each scenario gets a fresh scenario
// struct (and with it a fresh world); steps are thin wrappers over the world
// and the fixtures so the behavioral wiring stays readable.
package bdd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

const (
	assertTimeout = 2 * time.Second       // event delivery is async; poll list/count assertions up to this long
	assertEvery   = 20 * time.Millisecond // polling interval for eventually
	// settleTime is the grace before asserting something did NOT happen — a
	// negative assertion can't poll for success. Hook→daemon→store round
	// trips measure well under 50ms locally, so 300ms is ~6× headroom.
	settleTime = 300 * time.Millisecond
)

// scenario is the per-scenario state: the hermetic world plus what the
// scenario has observed so far.
type scenario struct {
	w          *world
	projects   map[string]string // project name in the feature → temp dir used as cwd
	cwds       map[string]string // native session id → the cwd it started in
	rows       []lsRow           // session list as of the last list assertion
	hookOut    string            // combined output of the most recent hook run
	hookErr    error             // exit outcome of the most recent hook run
	sessionID  string            // short id of "the session" from a Given
	eventCount int               // baseline event count captured by the Given
}

func initializeScenario(sc *godog.ScenarioContext) {
	s := &scenario{projects: map[string]string{}, cwds: map[string]string{}}

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
	sc.Given(`^the session manager is not running$`, s.sessionManagerNotRunning)
	sc.Given(`^a claude session with native id "([^"]*)" has started in "([^"]*)"$`, s.claudeSessionHasStarted)
	sc.Step(`^a claude session with native id "([^"]*)" starts in "([^"]*)"$`, s.claudeSessionStarts)
	sc.Step(`^a claude session with native id "([^"]*)" starts again as a resume$`, s.claudeSessionResumes)
	sc.Step(`^a claude session with native id "([^"]*)" starts again after a /clear$`, s.claudeSessionClears)
	sc.Step(`^a claude session with native id "([^"]*)" restarts due to context compaction$`, s.claudeSessionCompacts)
	sc.Step(`^an opencode session with native id "([^"]*)" starts in "([^"]*)"$`, s.opencodeSessionStarts)
	sc.Step(`^an agent sends a start notification that is not valid JSON$`, s.agentSendsInvalidJSON)
	sc.Step(`^the hook exits successfully$`, s.hookExitedSuccessfully)
	sc.Step(`^the session list contains exactly (\d+) sessions?$`, s.sessionListContainsExactly)
	sc.Step(`^that session is a "([^"]*)" session in "([^"]*)"$`, s.thatSessionIs)
	sc.Step(`^that session's state is "([^"]*)"$`, s.thatSessionStateIs)
	sc.Step(`^the list contains an "([^"]*)" session in "([^"]*)" with state "([^"]*)"$`, s.listContainsSession)
	sc.Step(`^the waiting-session count is (\d+)$`, s.waitingCountIs)
	sc.Step(`^the event count for the session is unchanged$`, s.eventCountUnchanged)
}

func (s *scenario) sessionManagerRunning() error {
	return s.w.start()
}

// sessionManagerNotRunning stops the daemon the Background started, so the
// hook fires against nothing listening.
func (s *scenario) sessionManagerNotRunning() error {
	return s.w.stop()
}

func (s *scenario) claudeSessionStarts(nativeID, project string) error {
	dir, err := s.projectDir(project)
	if err != nil {
		return err
	}
	s.cwds[nativeID] = dir
	return s.runHook("claude", claudeSessionStartJSON(nativeID, dir, "startup"))
}

// claudeSessionHasStarted is the Given form: it starts the session and waits
// until both the session row and its start event are visible, capturing the
// baselines later "unchanged" assertions compare against.
func (s *scenario) claudeSessionHasStarted(nativeID, project string) error {
	if err := s.claudeSessionStarts(nativeID, project); err != nil {
		return err
	}
	if err := s.sessionListContainsExactly(1); err != nil {
		return err
	}
	s.sessionID = s.rows[0].id
	return s.eventually(func() error {
		n, err := s.countEvents(s.sessionID)
		if err != nil {
			return err
		}
		if n < 1 {
			return fmt.Errorf("start event not yet recorded for session %s", s.sessionID)
		}
		s.eventCount = n
		return nil
	})
}

func (s *scenario) claudeSessionResumes(nativeID string) error {
	return s.restart(nativeID, "resume")
}

func (s *scenario) claudeSessionClears(nativeID string) error {
	return s.restart(nativeID, "clear")
}

func (s *scenario) claudeSessionCompacts(nativeID string) error {
	return s.restart(nativeID, "compact")
}

func (s *scenario) restart(nativeID, source string) error {
	dir, ok := s.cwds[nativeID]
	if !ok {
		return fmt.Errorf("session %q never started in this scenario", nativeID)
	}
	return s.runHook("claude", claudeSessionStartJSON(nativeID, dir, source))
}

func (s *scenario) opencodeSessionStarts(nativeID, project string) error {
	dir, err := s.projectDir(project)
	if err != nil {
		return err
	}
	s.cwds[nativeID] = dir
	return s.runHook("opencode", opencodeSessionStartJSON(nativeID, dir))
}

// agentSendsInvalidJSON records the hook outcome without judging it; the
// scenario's Then steps assert the contract (exit 0, no session).
func (s *scenario) agentSendsInvalidJSON() error {
	s.hookOut, s.hookErr = s.w.sm(strings.NewReader(`{this is not json`), "hook", "claude")
	return nil
}

func (s *scenario) hookExitedSuccessfully() error {
	if s.hookErr != nil {
		return fmt.Errorf("hook exited with error: %v\n%s", s.hookErr, s.hookOut)
	}
	return nil
}

func (s *scenario) sessionListContainsExactly(n int) error {
	if n == 0 {
		// No positive signal to poll for: give any in-flight event time to
		// land, then assert nothing appeared.
		time.Sleep(settleTime)
	}
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

// thatSessionStateIs re-polls rather than trusting the snapshot a count-only
// assertion cached: a session can win the count race while its state is
// still settling.
func (s *scenario) thatSessionStateIs(state string) error {
	return s.eventually(func() error {
		rows, err := s.listSessions()
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return fmt.Errorf(`"that session" needs exactly 1 listed session, have %d`, len(rows))
		}
		s.rows = rows
		if rows[0].status != state {
			return fmt.Errorf("session state is %q, want %q", rows[0].status, state)
		}
		return nil
	})
}

func (s *scenario) listContainsSession(agent, project, state string) error {
	return s.eventually(func() error {
		rows, err := s.listSessions()
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.agent == agent && row.cwd == s.projects[project] && row.status == state {
				return nil
			}
		}
		return fmt.Errorf("no %q session in %q with state %q among %+v", agent, project, state, rows)
	})
}

// waitingCountIs reads the alert-sink file the daemon maintains for status
// bars. The file must exist — absence means the sink never fired, which is a
// failure, not a pass. The path mirrors the sink wiring in daemon.Run
// ($XDG_STATE_HOME/sm/waiting-count); if that moves, this must follow.
func (s *scenario) waitingCountIs(n int) error {
	path := filepath.Join(s.w.stateDir, "sm", "waiting-count")
	return s.eventually(func() error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("waiting-count file: %w", err)
		}
		if got := strings.TrimSpace(string(b)); got != strconv.Itoa(n) {
			return fmt.Errorf("waiting-count is %q, want %d", got, n)
		}
		return nil
	})
}

// eventCountUnchanged asserts no event landed since the Given captured its
// baseline. A NOT-assertion can't poll for success, so it settles first.
func (s *scenario) eventCountUnchanged() error {
	time.Sleep(settleTime)
	n, err := s.countEvents(s.sessionID)
	if err != nil {
		return err
	}
	if n != s.eventCount {
		return fmt.Errorf("event count went from %d to %d; an event reached the daemon", s.eventCount, n)
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

// runHook pipes hook JSON into `sm hook <agent>`, recording the outcome. The
// hook contract is that it never breaks the agent, so a non-zero exit fails
// the step immediately.
func (s *scenario) runHook(agent, input string) error {
	s.hookOut, s.hookErr = s.w.sm(strings.NewReader(input), "hook", agent)
	if s.hookErr != nil {
		return fmt.Errorf("sm hook %s: %v\n%s", agent, s.hookErr, s.hookOut)
	}
	return nil
}

// eventually polls check until it succeeds or the assertion deadline passes;
// the scenario flavor attaches the daemon's stderr to the last failure.
func (s *scenario) eventually(check func() error) error {
	if err := eventually(check); err != nil {
		return fmt.Errorf("%w\ndaemon stderr:\n%s", err, s.w.daemonStderr())
	}
	return nil
}

// eventually is the package-level polling primitive shared by step
// definitions and harness tests.
func eventually(check func() error) error {
	deadline := time.Now().Add(assertTimeout)
	for {
		err := check()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
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
	return parseLS(out)
}

// parseLS splits `sm ls` output into rows by whitespace. Field indices
// follow the header (ID AGENT STATUS STARTED LAST CWD): STARTED and LAST
// are "date time" pairs, putting CWD at field 7. CWDs in this suite come
// from os.MkdirTemp, so they never contain spaces.
func parseLS(out string) ([]lsRow, error) {
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

var eventCountRe = regexp.MustCompile(`Events \((\d+),`)

// countEvents reads a session's recorded event count from `sm show`.
func (s *scenario) countEvents(id string) (int, error) {
	out, err := s.w.sm(nil, "show", id)
	if err != nil {
		return 0, fmt.Errorf("sm show %s: %v\n%s", id, err, out)
	}
	if strings.Contains(out, "(no events)") {
		return 0, nil
	}
	m := eventCountRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no event count in sm show output:\n%s", out)
	}
	return strconv.Atoi(m[1])
}
