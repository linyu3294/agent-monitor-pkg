package tui

import (
	"os"
	"os/exec"
	"path/filepath"
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
