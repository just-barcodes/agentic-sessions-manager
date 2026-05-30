# Notes

- want a statusbar in my waybar that shows active / waiting agent sessions
- need timestamps on all events, and a way to query event history per session
- handle `/clear` events which do not send SessionEnd events
- setting for each session if alert should be fired on waiting or finished, or both
- check https://code.claude.com/docs/en/hooks for hooks docs
- add _failed_ status (for opencode)
- capture LastPrompt for opencode sessions (claude hook does it now; opencode plugin events don't carry prompt text)
- `sm emit` contends on the DB lock while the daemon runs (SQLITE_BUSY) — route it through the bus like the hooks, or add a busy-timeout
