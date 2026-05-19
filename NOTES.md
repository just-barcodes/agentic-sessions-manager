# Notes

- want a statusbar in my waybar that shows active / waiting agent sessions
- handle `/clear` events which do not send SessionEnd events
- setting for each session if alert should be fired on waiting or finished, or both
- check https://code.claude.com/docs/en/hooks for hooks docs
- capture LastPrompt for opencode sessions (claude hook does it now; opencode plugin events don't carry prompt text)
- add _failed_ status (for opencode)
- detect idle after timeout
- add something like a `sm reset` or `sm clean` command to clean up old sessions
