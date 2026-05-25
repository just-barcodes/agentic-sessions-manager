package hook

import (
	"testing"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

func TestClaudeKindSessionEnd(t *testing.T) {
	kind, ok := claudeKind("SessionEnd")
	if !ok {
		t.Fatal("SessionEnd hook not recognized")
	}
	if kind != session.EventSessionEnd {
		t.Fatalf("SessionEnd mapped to %q, want %q", kind, session.EventSessionEnd)
	}
	if got := session.NextState(kind); got != session.StateFinished {
		t.Errorf("SessionEnd transitions to %q, want %q", got, session.StateFinished)
	}
}
