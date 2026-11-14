package tui

import (
	"strings"
	"testing"
	"time"

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

// renderTestModel builds a model sized like a real terminal so the card
// renderer runs its full path instead of the pre-WindowSizeMsg stub.
func renderTestModel(sessions ...session.Session) *model {
	m := New(Options{RefreshInterval: 2 * time.Second, IdleThreshold: 20 * time.Minute, MaxCards: 50}, "test")
	m.width, m.height = 120, 40
	m.sessions = sessions
	return m
}

// A Codewhale card must be visually distinguishable from a Cursor one,
// and its cmd row must name the CLI that can actually resume it.
func TestRenderCardCodewhaleBadgeAndCommand(t *testing.T) {
	m := renderTestModel(session.Session{
		ID:         "caa88777-1a40-435a-a2b2-fd861ab744a2",
		Source:     session.SourceCodewhale,
		Project:    "agent-monitor-pkg",
		ProjectDir: "Users-u-Documents-agent-monitor-pkg",
		Mtime:      time.Now().Unix(),
		LastRole:   session.RoleAssistant,
		LastTool:   "bash",
		LastText:   "working on the cards",
		State:      session.StateActive,
	})
	out := m.View()

	for _, want := range []string{"codewhale", "codewhale resume caa88777", "agent-monitor-pkg", "ACTIVE"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered card missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "cursor-agent --resume") {
		t.Errorf("codewhale card shows the cursor resume command:\n%s", out)
	}
}

// A Cursor card keeps its original command and carries no codewhale
// badge, so the two sources stay distinguishable.
func TestRenderCardCursorUnchanged(t *testing.T) {
	m := renderTestModel(session.Session{
		ID:         "afd3a58d-8fb9-40ad-b3da-c88da1e575dc",
		Source:     session.SourceCursor,
		Project:    "credit-platform-worktrees-feat-bionic",
		ProjectDir: "Users-u-Documents-credit-platform-worktrees-feat-bionic",
		Mtime:      time.Now().Unix(),
		LastRole:   session.RoleAssistant,
		LastTool:   "Shell",
		State:      session.StateActive,
	})
	out := m.View()

	if !strings.Contains(out, "cursor-agent --resume afd3a58d") {
		t.Errorf("cursor card missing its resume command:\n%s", out)
	}
	if strings.Contains(out, "codewhale") {
		t.Errorf("cursor card carries a codewhale badge:\n%s", out)
	}
}
