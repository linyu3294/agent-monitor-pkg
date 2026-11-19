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

// TestNewWorktreeApprovalDefaultsToAsk is the regression guard for the
// launch posture: a new agent must ask the user before acting, and only
// run unattended if the user explicitly selects it.
func TestNewWorktreeApprovalDefaultsToAsk(t *testing.T) {
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "codewhale"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")
	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)

	if mm.form.approval != wtbridge.ApprovalOnRequest {
		t.Fatalf("default approval = %v, want on-request", mm.form.approval)
	}
	view := mm.View()
	if testing.Verbose() {
		t.Logf("modal:\n%s", view)
	}
	if !strings.Contains(view, "on-request") {
		t.Fatalf("modal does not show the approval row:\n%s", view)
	}
	if strings.Contains(view, "will not stop to ask you") {
		t.Fatal("modal warns about unattended mode before it was selected")
	}

	// Tab until the approval row has focus. The starting field depends on
	// whether a repo could be inferred from the selected card, so the
	// test must not hardcode it.
	for i := 0; mm.form.focused != 4; i++ {
		if i > newWtFields {
			t.Fatalf("never reached the approval row (focused = %d)", mm.form.focused)
		}
		mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyTab})
		mm = mdl.(*model)
	}
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRight})
	mm = mdl.(*model)
	if mm.form.approval != wtbridge.ApprovalAuto {
		t.Fatalf("approval after → = %v, want auto", mm.form.approval)
	}
	if !strings.Contains(mm.View(), "auto") {
		t.Fatalf("modal does not render the auto posture:\n%s", mm.View())
	}
	if testing.Verbose() {
		t.Logf("auto:\n%s", mm.View())
	}
	if strings.Contains(mm.View(), "will not stop to ask you") {
		t.Fatal("auto must not carry the unattended warning")
	}

	// One more step is the unattended posture, which must warn.
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRight})
	mm = mdl.(*model)
	if mm.form.approval != wtbridge.ApprovalNever {
		t.Fatalf("approval after →→ = %v, want never", mm.form.approval)
	}
	if !strings.Contains(mm.View(), "will not stop to ask you") {
		t.Fatalf("no warning after choosing unattended:\n%s", mm.View())
	}

	// The chosen posture reaches the launcher: never -> explicit never.
	cmd := mm.form.agent.LaunchCommand("", mm.form.approval)
	if !strings.Contains(cmd, "--approval-policy never") {
		t.Fatalf("launch command = %q, want --approval-policy never", cmd)
	}
}

// creating a worktree used to fail with "fatal: invalid reference:
// origin/main" on any repo without that ref — no remote, or a master
// default. The modal must detect a base that exists and create.
func TestNewWorktreeCreatesOnRepoWithoutOriginMain(t *testing.T) {
	repo := wtbridge.GitPrimary(initGitRepo(t))
	if repo == "" {
		t.Fatal("temp repo has no git primary")
	}
	if refExistsForTest(t, repo, "origin/main") {
		t.Fatal("test repo unexpectedly has origin/main")
	}

	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "codewhale"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "osascript"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))

	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")
	m.sessions = []session.Session{{
		ID:         "abcdef1234567890",
		Source:     session.SourceCodewhale,
		ProjectDir: repoToEncoded(repo),
		CWDHint:    repo,
	}}

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)

	// The detected base must be a ref that exists, not a hardcoded guess.
	if got := mm.form.base.Value(); got != "main" {
		t.Fatalf("base prefill = %q, want main", got)
	}

	mm.form.branch.SetValue("feature-noremote")
	mdl, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("expected create command, form err=%q", mm.form.err)
	}
	msg := cmd()
	if res, ok := msg.(wtResultMsg); ok && res.failed {
		t.Fatalf("create failed: %s / %s", res.title, res.subtitle)
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feature-noremote")); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
}

// refExistsForTest reports whether a ref resolves in repo.
func refExistsForTest(t *testing.T, repo, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return cmd.Run() == nil
}

// repoToEncoded builds the encoded project dir the scanner would use.
func repoToEncoded(repo string) string {
	return scanner.EncodeForCursor(strings.TrimPrefix(repo, "/"))
}

// TestNewWorktreeUsesConfiguredDefaults covers AM_DEFAULT_*. The operator sets
// these once so an ordinary launch already has egress and the SSO login, instead
// of retyping both every time.
func TestNewWorktreeUsesConfiguredDefaults(t *testing.T) {
	m := New(Options{
		RefreshInterval: time.Second,
		IdleThreshold:   time.Hour,
		MaxCards:        10,
		DefaultApproval: wtbridge.ApprovalNever,
		DefaultNetwork:  wtbridge.NetworkOn,
		DefaultBefore:   "aws sso login --profile bora",
	}, "test")

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)

	if mm.form.approval != wtbridge.ApprovalNever {
		t.Fatalf("approval = %v, want never", mm.form.approval)
	}
	if mm.form.network != wtbridge.NetworkOn {
		t.Fatalf("network = %v, want on", mm.form.network)
	}
	if got := mm.form.before.Value(); got != "aws sso login --profile bora" {
		t.Fatalf("before = %q, want the configured default", got)
	}
	// The default is visible, not silent: a launch that reaches the network has
	// to say so on the screen the operator is confirming.
	if !strings.Contains(mm.View(), "sandbox is disabled") {
		t.Fatalf("configured egress default does not warn:\n%s", mm.View())
	}
}

// TestNewWorktreeDefaultSurvivesOneOffEdit keeps the configured default from
// being destroyed by editing the field for a single launch: the next `n` asks
// the operator again rather than silently launching without the setup command.
func TestNewWorktreeDefaultSurvivesOneOffEdit(t *testing.T) {
	m := New(Options{
		RefreshInterval: time.Second,
		IdleThreshold:   time.Hour,
		MaxCards:        10,
		DefaultBefore:   "aws sso login --profile bora",
	}, "test")

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)

	mm.form.before.SetValue("")
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = mdl.(*model)

	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm = mdl.(*model)
	if got := mm.form.before.Value(); got != "aws sso login --profile bora" {
		t.Fatalf("before = %q after reopening, want the configured default restored", got)
	}
}

// TestNewWorktreeNetworkDefaultsOffAndWarns is the regression guard for egress:
// a new agent is confined unless the person launching it asks otherwise, and the
// modal says plainly what turning it on costs.
func TestNewWorktreeNetworkDefaultsOffAndWarns(t *testing.T) {
	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)

	if mm.form.network != wtbridge.NetworkOff {
		t.Fatalf("default network = %v, want off", mm.form.network)
	}
	if view := mm.View(); !strings.Contains(view, "net:") || !strings.Contains(view, "workspace-write") {
		t.Fatalf("modal does not render the confined net row:\n%s", view)
	}
	if strings.Contains(mm.View(), "sandbox is disabled") {
		t.Fatal("modal warns about full access before it was selected")
	}

	// Tab until the net row has focus; the entry point depends on whether a
	// repo could be inferred from the selected card.
	for i := 0; mm.form.focused != 5; i++ {
		if i > newWtFields {
			t.Fatalf("never reached the net row (focused = %d)", mm.form.focused)
		}
		mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyTab})
		mm = mdl.(*model)
	}

	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRight})
	mm = mdl.(*model)
	if mm.form.network != wtbridge.NetworkOn {
		t.Fatalf("network after → = %v, want on", mm.form.network)
	}
	view := mm.View()
	if !strings.Contains(view, "full access") {
		t.Fatalf("modal does not label the egress posture:\n%s", view)
	}
	if !strings.Contains(view, "sandbox is disabled") {
		t.Fatalf("no warning after choosing full access:\n%s", view)
	}
	if strings.Contains(view, "will not stop to ask you") {
		t.Fatal("the egress warning must not be the unattended warning")
	}

	// ← wraps back to the confined value rather than off the end.
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyLeft})
	mm = mdl.(*model)
	if mm.form.network != wtbridge.NetworkOff {
		t.Fatalf("network after ← = %v, want off", mm.form.network)
	}

	// The approval posture is untouched by all of this.
	if mm.form.approval != wtbridge.ApprovalOnRequest {
		t.Fatalf("approval = %v, want on-request", mm.form.approval)
	}
}

// TestNewWorktreeBeforeSitsBeforePrompt pins the setup row's place in the tab
// order and proves keystrokes land there rather than leaking into the prompt.
func TestNewWorktreeBeforeSitsBeforePrompt(t *testing.T) {
	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)

	if !strings.Contains(mm.View(), "before:") {
		t.Fatalf("modal does not render the before row:\n%s", mm.View())
	}

	for i := 0; mm.form.focused != 6; i++ {
		if i > newWtFields {
			t.Fatalf("never reached the before row (focused = %d)", mm.form.focused)
		}
		mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyTab})
		mm = mdl.(*model)
	}

	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("aws sso login")})
	mm = mdl.(*model)
	if mm.form.before.Value() != "aws sso login" {
		t.Fatalf("before = %q, want the typed command", mm.form.before.Value())
	}
	if mm.form.prompt.Value() != "" {
		t.Fatalf("prompt = %q, want it untouched", mm.form.prompt.Value())
	}

	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyTab})
	mm = mdl.(*model)
	if mm.form.focused != 7 {
		t.Fatalf("focused = %d after before, want 7 (prompt)", mm.form.focused)
	}
	mdl, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ship it")})
	mm = mdl.(*model)
	if mm.form.prompt.Value() != "ship it" || mm.form.before.Value() != "aws sso login" {
		t.Fatalf("prompt=%q before=%q — fields did not stay separate", mm.form.prompt.Value(), mm.form.before.Value())
	}
}
