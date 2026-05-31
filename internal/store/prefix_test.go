package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/just-barcodes/agentic-sessions-manager/internal/session"
)

func newPrefixStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sm.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestResolveSessionIDByPrefix(t *testing.T) {
	st := newPrefixStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	for _, id := range []string{"aaaa1111", "aaaa2222", "bbbb3333"} {
		if err := st.CreateSession(ctx, session.Session{
			ID: id, Agent: "claude", CWD: "/x", HostID: "h",
			StartedAt: now, LastEventAt: now, Status: session.StateRunning,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	t.Run("exact", func(t *testing.T) {
		got, err := st.ResolveSessionID(ctx, "bbbb3333")
		if err != nil || got != "bbbb3333" {
			t.Fatalf("got (%q,%v), want bbbb3333", got, err)
		}
	})
	t.Run("unique prefix", func(t *testing.T) {
		got, err := st.ResolveSessionID(ctx, "bbbb")
		if err != nil || got != "bbbb3333" {
			t.Fatalf("got (%q,%v), want bbbb3333", got, err)
		}
	})
	t.Run("ambiguous prefix", func(t *testing.T) {
		if _, err := st.ResolveSessionID(ctx, "aaaa"); err == nil {
			t.Fatal("expected ambiguous-prefix error")
		}
	})
	t.Run("no match", func(t *testing.T) {
		if _, err := st.ResolveSessionID(ctx, "zzzz"); err == nil {
			t.Fatal("expected no-match error")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := st.ResolveSessionID(ctx, ""); err == nil {
			t.Fatal("expected empty-id error")
		}
	})
}
