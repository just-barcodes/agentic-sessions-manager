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
