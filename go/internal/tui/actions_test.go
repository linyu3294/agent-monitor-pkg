package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agent-monitor/agent-monitor/internal/scanner"
	"github.com/agent-monitor/agent-monitor/internal/session"
)

// These tests drive the real key handlers against a real git repo with
// no `wt` binary on the machine, which is the configuration that was
// failing: remove, sync, and attach all returned "wt binary unavailable".

func guiStubPATH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	for _, name := range []string{"osascript", "open"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))
}

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// tuiRepoWithWorktree builds a repo plus a .worktrees/<slug> checkout and
// returns the encoded session fields the scanner would produce for it.
func tuiRepoWithWorktree(t *testing.T, slug string) (primary string, sess session.Session) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	primary = filepath.Join(dir, "repo")
	gitTest(t, dir, "init", "-b", "main", primary)
	if err := os.WriteFile(filepath.Join(primary, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, primary, "add", "README")
	gitTest(t, primary, "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "-m", "init")
	gitTest(t, primary, "worktree", "add", "-b", slug, filepath.Join(primary, ".worktrees", slug), "main")

	encodedPrimary := scanner.EncodeForCursor(strings.TrimPrefix(primary, "/"))
	encodedSlug := scanner.EncodeForCursor(slug)
	sess = session.Session{
		ID:           "abcdef1234567890",
		Source:       session.SourceCodewhale,
		ProjectDir:   encodedPrimary + "-worktrees-" + encodedSlug,
		Project:      "repo",
		WorktreeSlug: encodedSlug,
		Mtime:        time.Now().Unix(),
		LastRole:     session.RoleAssistant,
		State:        session.StateActive,
		// Codewhale logs state the real path; the scanner fills this in.
		CWDHint: filepath.Join(primary, ".worktrees", slug),
	}
	return primary, sess
}

// drive sends keys through Update and returns the model plus the command
// produced by the final key (nil when the key was handled inline).
func drive(t *testing.T, m *model, keys ...tea.KeyMsg) (*model, tea.Cmd) {
	t.Helper()
	var mdl tea.Model = m
	var last tea.Cmd
	for _, k := range keys {
		mdl, last = mdl.Update(k)
	}
	return mdl.(*model), last
}

// execCmd runs a cmd, expanding tea.Batch, and returns every message.
func execCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, execCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// results runs cmd and returns the wtResultMsg values it produced.
func results(cmd tea.Cmd) []wtResultMsg {
	var out []wtResultMsg
	for _, msg := range execCmd(cmd) {
		if r, ok := msg.(wtResultMsg); ok {
			out = append(out, r)
		}
	}
	return out
}

func newTestModel(sess session.Session) *model {
	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")
	m.width, m.height = 120, 40
	m.sessions = []session.Session{sess}
	return m
}

// `r` then `y` must remove the worktree and its branch with no wt.
func TestRemoveKeyWithoutWt(t *testing.T) {
	guiStubPATH(t)
	primary, sess := tuiRepoWithWorktree(t, "feat-rm")

	m, cmd := drive(t, newTestModel(sess),
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}},
	)
	res := results(cmd)
	if len(res) == 0 {
		t.Fatal("remove produced no result message")
	}
	for _, r := range res {
		if r.failed {
			t.Fatalf("remove failed: %s", r.subtitle)
		}
	}
	if _, err := os.Stat(filepath.Join(primary, ".worktrees", "feat-rm")); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
	if m.confirm.title == "" {
		t.Fatal("no confirmation was shown")
	}
}

// A failing remove must report failure and lift the tombstone so the
// card comes back rather than silently vanishing.
func TestRemoveFailureRestoresCard(t *testing.T) {
	guiStubPATH(t)
	primary, sess := tuiRepoWithWorktree(t, "feat-gone")
	if err := os.RemoveAll(filepath.Join(primary, ".worktrees", "feat-gone")); err != nil {
		t.Fatal(err)
	}

	m, cmd := drive(t, newTestModel(sess),
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}},
	)
	key := tombstoneKey(sess)
	if _, ok := m.tombstones[key]; !ok {
		t.Fatal("card was not tombstoned before removal")
	}
	failed := false
	for _, r := range results(cmd) {
		if r.failed {
			failed = true
			if r.restore != key {
				t.Fatalf("failed result restore = %q, want %q", r.restore, key)
			}
		}
	}
	if !failed {
		t.Fatal("removing a missing worktree reported success")
	}

	// The restore must actually clear the tombstone when it arrives.
	_, _ = m.Update(wtResultMsg{title: "removed", failed: true, restore: key})
	if _, ok := m.tombstones[key]; ok {
		t.Fatal("tombstone was not lifted after a failed remove")
	}
}

// `s` must run a real rebase with no wt.
func TestSyncKeyWithoutWt(t *testing.T) {
	guiStubPATH(t)
	primary, sess := tuiRepoWithWorktree(t, "feat-sync")
	dest := filepath.Join(primary, ".worktrees", "feat-sync")

	if err := os.WriteFile(filepath.Join(dest, "w.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dest, "add", "w.txt")
	gitTest(t, dest, "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "-m", "work")

	_, cmd := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	res := results(cmd)
	if len(res) == 0 {
		t.Fatal("sync produced no result message")
	}
	for _, r := range res {
		if r.failed {
			t.Fatalf("sync failed: %s", r.subtitle)
		}
	}
}

// `a` must open a resume tab with no wt.
func TestAttachKeyWithoutWt(t *testing.T) {
	guiStubPATH(t)
	_, sess := tuiRepoWithWorktree(t, "feat-attach")

	_, cmd := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	res := results(cmd)
	if len(res) == 0 {
		t.Fatal("attach produced no result message")
	}
	for _, r := range res {
		if r.failed {
			t.Fatalf("attach failed: %s", r.subtitle)
		}
	}
}
