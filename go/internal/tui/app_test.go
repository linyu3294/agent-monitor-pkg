package tui

import (
	"testing"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

func TestTombstoneKeyDistinguishesStubWorktreeCards(t *testing.T) {
	a := session.Session{ID: "(no session)", ProjectDir: "Users-me-repo-worktrees-feat-a", WorktreeSlug: "feat-a"}
	b := session.Session{ID: "(no session)", ProjectDir: "Users-me-repo-worktrees-feat-b", WorktreeSlug: "feat-b"}
	if tombstoneKey(a) == tombstoneKey(b) {
		t.Fatalf("stub cards for different worktrees share tombstone key %q", tombstoneKey(a))
	}
}

func TestTombstoneKeyPrimaryUsesChatID(t *testing.T) {
	s := session.Session{ID: "abc", ProjectDir: "Users-me-repo"}
	if got := tombstoneKey(s); got != "abc" {
		t.Fatalf("tombstoneKey = %q, want chat id", got)
	}
}

func TestTombstoneKeyWorktreeCoversAllChats(t *testing.T) {
	older := session.Session{ID: "old", ProjectDir: "Users-me-repo-worktrees-feat-a", WorktreeSlug: "feat-a"}
	newer := session.Session{ID: "new", ProjectDir: "Users-me-repo-worktrees-feat-a", WorktreeSlug: "feat-a"}
	if tombstoneKey(older) != tombstoneKey(newer) {
		t.Fatal("removing a worktree card must also hide older chats on that worktree")
	}
}
