// Hook-input fixtures: the agent-side JSON the BDD steps pipe into
// `sm hook <agent>`, mirroring what Claude Code and opencode actually send.
package bdd

import "fmt"

// claudeSessionStartJSON is the subset of Claude Code's SessionStart hook
// stdin that sm consumes. source is startup|resume|clear|compact.
func claudeSessionStartJSON(nativeID, cwd, source string) string {
	return fmt.Sprintf(`{"session_id":%q,"hook_event_name":"SessionStart","source":%q,"cwd":%q}`,
		nativeID, source, cwd)
}

// opencodeSessionStartJSON is the subset of an opencode session.created
// plugin event that sm consumes.
func opencodeSessionStartJSON(nativeID, dir string) string {
	return fmt.Sprintf(`{"type":"session.created","properties":{"sessionID":%q,"info":{"directory":%q}}}`,
		nativeID, dir)
}
