Feature: Knowing when a new agent session starts
  As someone running multiple AI coding agents
  I want sm to notice the moment a new session starts
  So that every live session is visible without me registering anything

  Background:
    Given the session manager is running

  Scenario: A brand-new session appears in the session list
    When a claude session with native id "sess-abc" starts in "project-a"
    Then the session list contains exactly 1 session
    And that session is a "claude" session in "project-a"
    And that session's state is "idle"

  Scenario: A newly started session does not demand attention
    When a claude session with native id "sess-abc" starts in "project-a"
    Then the waiting-session count is 0

  Scenario: A session that starts again after a resume is not listed twice
    Given a claude session with native id "sess-abc" has started in "project-a"
    When a claude session with native id "sess-abc" starts again as a resume
    Then the session list contains exactly 1 session
    And that session's state is "idle"

  Scenario: A session that starts again after a /clear is not listed twice
    Given a claude session with native id "sess-abc" has started in "project-a"
    When a claude session with native id "sess-abc" starts again after a /clear
    Then the session list contains exactly 1 session
    And that session's state is "idle"

  Scenario: A context-compaction restart never reaches the session manager
    Given a claude session with native id "sess-abc" has started in "project-a"
    When a claude session with native id "sess-abc" restarts due to context compaction
    Then the hook exits successfully
    And the event count for the session is unchanged
    And the session list contains exactly 1 session

  Scenario: Sessions from different agents are tracked separately
    When a claude session with native id "sess-abc" starts in "project-a"
    And an opencode session with native id "oc-1" starts in "project-b"
    Then the session list contains exactly 2 sessions
    And the list contains an "opencode" session in "project-b" with state "idle"

  Scenario: Invalid agent output creates no session
    When an agent sends a start notification that is not valid JSON
    Then the hook exits successfully
    And the session list contains exactly 0 sessions

  Scenario: A hook fired while the session manager is down does not disturb the agent
    Given the session manager is not running
    When a claude session with native id "sess-abc" starts in "project-a"
    Then the hook exits successfully
