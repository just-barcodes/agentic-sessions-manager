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

// claudeUserPromptJSON is a UserPromptSubmit hook payload. Claude carries the
// session's AI-generated title on this event (and on SessionStart); title is
// empty until Claude has named the session.
func claudeUserPromptJSON(nativeID, prompt, title string) string {
	return fmt.Sprintf(`{"session_id":%q,"hook_event_name":"UserPromptSubmit","prompt":%q,"session_title":%q}`,
		nativeID, prompt, title)
}

// claudeStopJSON is a Stop hook payload. Stop carries no session_title, so the
// only title on it is whatever sm reads from the transcript at transcriptPath.
func claudeStopJSON(nativeID, transcriptPath string) string {
	return fmt.Sprintf(`{"session_id":%q,"hook_event_name":"Stop","transcript_path":%q}`,
		nativeID, transcriptPath)
}

// claudeTranscriptJSONL is a minimal transcript: one ordinary line plus the
// ai-title record Claude writes once it has named the session.
func claudeTranscriptJSONL(nativeID, title string) string {
	return fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q}\n{\"type\":\"ai-title\",\"aiTitle\":%q,\"sessionId\":%q}\n",
		nativeID, title, nativeID)
}

// opencodePermissionJSON is the opencode event fired when a session blocks on a
// permission request.
func opencodePermissionJSON(nativeID string) string {
	return fmt.Sprintf(`{"type":"permission.asked","properties":{"sessionID":%q}}`, nativeID)
}

// opencodeTitleJSON is the session.title event the sm plugin synthesises when
// opencode renames a session it has already announced.
func opencodeTitleJSON(nativeID, title string) string {
	return fmt.Sprintf(`{"type":"session.title","properties":{"sessionID":%q,"info":{"title":%q}}}`,
		nativeID, title)
}
