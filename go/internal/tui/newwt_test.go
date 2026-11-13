package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agent-monitor/agent-monitor/internal/session"
	"github.com/agent-monitor/agent-monitor/internal/wtbridge"
)

func TestNewWorktreeInfersRepoFromShellCWD(t *testing.T) {
	repo := wtbridge.GitPrimary(initGitRepo(t))
	if repo == "" {
		t.Fatal("temp repo has no git primary")
	}
	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	body := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"git status","working_directory":"` + repo + `"}}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))
	t.Setenv("WT_NO_OPEN", "1")

	m := New(Options{
		RefreshInterval: time.Second,
		IdleThreshold:   time.Hour,
		MaxCards:        10,
	}, "test")
	m.sessions = []session.Session{{
		ID:             "abcdef1234567890",
		ProjectDir:     "not-a-git-workspace",
		TranscriptPath: transcript,
	}}

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)
	if mm.state != stateNewWt {
		t.Fatalf("state = %v", mm.state)
	}
	if mm.form.repo.Value() != repo {
		t.Fatalf("repo field = %q, want %q", mm.form.repo.Value(), repo)
	}
	mm.form.branch.SetValue("feature-n")
	mm.form.base.SetValue("main")
	var cmd tea.Cmd
	mdl, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected create command")
	}
	msg := cmd()
	mdl, _ = mdl.Update(msg)
	mm = mdl.(*model)
	if mm.state != stateList {
		t.Fatalf("state after enter = %v err=%q", mm.state, mm.form.err)
	}
	res, ok := msg.(wtResultMsg)
	if !ok {
		t.Fatalf("msg type %T", msg)
	}
	if res.failed {
		t.Fatalf("create failed: %s / %s", res.title, res.subtitle)
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feature-n")); err != nil {
		t.Fatal(err)
	}
}

// TestNewWorktreeSelectsCodewhale drives the agent selector through the
// real key path: `n`, tab to the agent row, →, then Enter.
func TestNewWorktreeSelectsCodewhale(t *testing.T) {
	repo := wtbridge.GitPrimary(initGitRepo(t))
	if repo == "" {
		t.Fatal("temp repo has no git primary")
	}
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "codewhale"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))
	t.Setenv("WT_NO_OPEN", "1")

	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")
	m.sessions = []session.Session{{ID: "abcdef1234567890", ProjectDir: repo}}

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)
	// Codewhale is on PATH in this test, so it is the default launcher.
	if mm.form.agent != wtbridge.AgentCodewhale {
		t.Fatalf("default agent = %v, want codewhale", mm.form.agent)
	}
	if !strings.Contains(mm.View(), "codewhale") {
		t.Fatal("modal does not render the default agent")
	}

	// Default focus is branch (1); tab twice lands on the agent row (3).
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyTab})
	mm = mdl.(*model)
	if mm.form.focused != 3 {
		t.Fatalf("focused = %d, want 3 (agent)", mm.form.focused)
	}
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyLeft})
	mm = mdl.(*model)
	if mm.form.agent != wtbridge.AgentCursor {
		t.Fatalf("agent after ← = %v, want cursor-agent", mm.form.agent)
	}
	if !strings.Contains(mm.View(), "cursor-agent") {
		t.Fatal("modal does not render the selected agent")
	}
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRight})
	mm = mdl.(*model)
	if mm.form.agent != wtbridge.AgentCodewhale {
		t.Fatalf("agent after → = %v, want codewhale", mm.form.agent)
	}

	mm.form.branch.SetValue("feature-wh")
	mm.form.base.SetValue("main")
	mdl, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected create command")
	}
	mdl, _ = mdl.Update(cmd())
	mm = mdl.(*model)
	if mm.state != stateList {
		t.Fatalf("state after enter = %v err=%q", mm.state, mm.form.err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feature-wh")); err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if !mm.showEmpty {
		t.Fatal("expected stub cards to be shown so the new codewhale worktree is visible")
	}
}

// TestNewWorktreeBlocksMissingAgent keeps the modal open, before any
// worktree exists, when the chosen launcher is not installed.
func TestNewWorktreeBlocksMissingAgent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))

	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")
	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)
	mm.form.repo.SetValue(t.TempDir())
	mm.form.branch.SetValue("feature-wh")
	mm.form.agent = wtbridge.AgentCodewhale

	mdl, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = mdl.(*model)
	if cmd != nil {
		t.Fatal("expected to stay in the modal")
	}
	if mm.state != stateNewWt {
		t.Fatalf("state = %v", mm.state)
	}
	if !strings.Contains(mm.form.err, "codewhale") || !strings.Contains(mm.form.err, "PATH") {
		t.Fatalf("err = %q, want a codewhale/PATH message", mm.form.err)
	}
	if mm.form.focused != 3 {
		t.Fatalf("focused = %d, want 3 (agent)", mm.form.focused)
	}
}

func TestNewWorktreeStaysOpenWhenRepoMissing(t *testing.T) {
	m := New(Options{
		RefreshInterval: time.Second,
		IdleThreshold:   time.Hour,
		MaxCards:        10,
	}, "test")
	m.sessions = []session.Session{{
		ID:         "abcdef1234567890",
		ProjectDir: "not-a-real-path-at-all",
	}}
	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)
	mm.form.branch.SetValue("feature-n")
	mdl, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = mdl.(*model)
	if cmd != nil {
		t.Fatal("expected to stay in the modal")
	}
	if mm.state != stateNewWt {
		t.Fatalf("state = %v", mm.state)
	}
	if mm.form.err == "" {
		t.Fatal("expected an inline error")
	}
	view := mm.View()
	if view == "" {
		t.Fatal("empty view")
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "README")
	run(t, dir, "git", "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "-m", "init")
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
