Feature: Knowing what each session is about
  As someone running multiple AI coding agents
  I want sm to show each session under the title its agent gave it
  So that I can tell my sessions apart without decoding directories

  Background:
    Given the session manager is running

  Scenario: A session the agent has named is listed under its title
    Given a claude session with native id "sess-abc" has started in "project-a"
    When the claude session "sess-abc" submits a prompt titled "Fix the flaky reaper test"
    Then the session list shows the title "Fix the flaky reaper test"

  Scenario: A session the agent has not named yet falls back to its directory
    Given a claude session with native id "sess-abc" has started in "project-a"
    When the claude session "sess-abc" submits a prompt with no title
    Then the session list contains exactly 1 session
    And that session is a "claude" session in "project-a"

  Scenario: A title the agent settles on mid-turn is read from the transcript
    Given a claude session with native id "sess-abc" has started in "project-a"
    When the claude session "sess-abc" finishes a turn titled "Capture session titles" in its transcript
    Then the session list shows the title "Capture session titles"

  Scenario: A renamed session is listed under its newest title
    Given a claude session with native id "sess-abc" has started in "project-a"
    When the claude session "sess-abc" submits a prompt titled "First idea"
    And the claude session "sess-abc" submits a prompt titled "Second idea"
    Then the session list shows the title "Second idea"

  Scenario: An untitled event never erases a title already known
    Given a claude session with native id "sess-abc" has started in "project-a"
    When the claude session "sess-abc" submits a prompt titled "Fix the flaky reaper test"
    And the claude session "sess-abc" submits a prompt with no title
    Then the session list shows the title "Fix the flaky reaper test"

  Scenario: Learning a title never disturbs what a session is doing
    Given an opencode session with native id "oc-1" is waiting for permission in "project-b"
    When opencode names session "oc-1" "Reset DB on machine for sm"
    Then the session list shows the title "Reset DB on machine for sm"
    And that session's state is "waiting"
