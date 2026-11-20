package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agent-monitor/agent-monitor/internal/scanner"
	"github.com/agent-monitor/agent-monitor/internal/session"
	"github.com/agent-monitor/agent-monitor/internal/wtbridge"
)

// claudeWorktreeSession builds a Claude Code card for a real worktree: a
// transcript on disk, the workspace stated by the transcript (Claude
// records it on every line), and the encoded project dir the scanner
// derives from that workspace.
func claudeWorktreeSession(t *testing.T, slug string) (primary string, sess session.Session) {
	t.Helper()
	primary, _ = tuiRepoWithWorktree(t, slug)
	id := "8b7c6d5e-4f3a-4b2c-9d1e-0a1b2c3d4e5f"
	worktree := filepath.Join(primary, ".worktrees", slug)

	dir := filepath.Join(t.TempDir(), "projects", "-tmp-repo-worktrees-"+scanner.EncodeForCursor(slug))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, id+".jsonl")
	body := `{"type":"assistant","cwd":"` + worktree + `","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return primary, session.Session{
		ID:             id,
		Source:         session.SourceClaude,
		ProjectDir:     scanner.EncodeForCursor(strings.TrimPrefix(worktree, "/")),
		Project:        "repo",
		WorktreeSlug:   scanner.EncodeForCursor(slug),
		TranscriptPath: transcript,
		CWDHint:        worktree,
		Mtime:          time.Now().Unix(),
		LastRole:       session.RoleAssistant,
		LastTool:       "Bash",
		State:          session.StateActive,
	}
}

// `a` on a Claude card must open `claude --resume <id>` in the worktree.
func TestClaudeAttach(t *testing.T) {
	log := guiStubPATH(t)
	primary, sess := claudeWorktreeSession(t, "feat-claude-a")

	_, cmd := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("attach failed: %s", r.subtitle)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no tab was opened: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "claude --resume "+sess.ID) {
		t.Fatalf("tab command = %q, want claude --resume <id>", text)
	}
	if !strings.Contains(text, filepath.Join(primary, ".worktrees", "feat-claude-a")) {
		t.Fatalf("tab did not cd into the worktree: %q", text)
	}
}

// A card must name the CLI that resumes it and carry a badge, so a Claude
// session is never mistaken for a Cursor one.
func TestRenderCardClaudeBadgeAndCommand(t *testing.T) {
	m := renderTestModel(session.Session{
		ID:         "8b7c6d5e-4f3a-4b2c-9d1e-0a1b2c3d4e5f",
		Source:     session.SourceClaude,
		Project:    "agent-monitor-pkg",
		ProjectDir: "Users-u-Documents-agent-monitor-pkg",
		Mtime:      time.Now().Unix(),
		LastRole:   session.RoleAssistant,
		LastTool:   "Bash",
		LastText:   "wiring the cards",
		State:      session.StateActive,
	})
	out := m.View()

	for _, want := range []string{"claude", "claude --resume 8b7c6d5e", "agent-monitor-pkg", "ACTIVE"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered card missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "cursor-agent --resume") {
		t.Errorf("claude card shows the cursor resume command:\n%s", out)
	}
}

// `f` tails a Claude transcript too; the bottom pane resolves the file
// through `am follow`, which now knows the Claude store.
func TestClaudeFollow(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> '" + log + "'\nexit 0\n"
	for _, name := range []string{"osascript", "open", "am"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))

	_, sess := claudeWorktreeSession(t, "feat-claude-f")
	_, cmd := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("follow failed: %s", r.subtitle)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("follow opened no tab: %v", err)
	}
	if !strings.Contains(string(data), "follow "+sess.ID) {
		t.Fatalf("follow did not open the tail tab: %q", data)
	}
}

// A machine with Claude Code but no codewhale defaults the `n` modal to
// claude — the cascade must not fall all the way back to cursor-agent.
func TestDefaultAgentPrefersClaude(t *testing.T) {
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))

	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")
	m.width, m.height = 120, 40
	if m.form.agent != wtbridge.AgentClaude {
		t.Fatalf("default agent = %v, want claude", m.form.agent)
	}

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)
	if mm.state != stateNewWt {
		t.Fatalf("state = %v, want the new-worktree modal", mm.state)
	}
	if v := mm.View(); !strings.Contains(v, "claude") {
		t.Fatalf("modal does not render the claude agent:\n%s", v)
	}
}

// Selecting `claude` in the modal must create the worktree and type the
// claude launch line into the new tab.
func TestNewWorktreeLaunchesClaude(t *testing.T) {
	repo := wtbridge.GitPrimary(initGitRepo(t))
	if repo == "" {
		t.Fatal("temp repo has no git primary")
	}
	// Stub the launcher and the GUI so the tab command is observable and
	// no real window opens. The rest of PATH stays intact: the repo setup
	// needs a working git.
	stubDir := t.TempDir()
	log := filepath.Join(stubDir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> '" + log + "'\nexit 0\n"
	for _, name := range []string{"claude", "osascript"} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))

	m := New(Options{RefreshInterval: time.Second, IdleThreshold: time.Hour, MaxCards: 10}, "test")
	m.sessions = []session.Session{{
		ID:         "abcdef1234567890",
		ProjectDir: repoToEncoded(repo),
		CWDHint:    repo,
	}}

	var mdl tea.Model = m
	mdl, _ = mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mdl, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mm := mdl.(*model)
	// Select claude the way the arrow keys would; which agent is the
	// default depends on the host, and TestDefaultAgentPrefersClaude
	// covers that separately.
	mm.form.agent = wtbridge.AgentClaude
	if !strings.Contains(mm.View(), "has no sandbox switch") {
		t.Fatalf("modal does not explain claude's missing net row:\n%s", mm.View())
	}

	mm.form.branch.SetValue("feature-claude")
	mdl, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("expected create command, form err=%q", mm.form.err)
	}
	if mm.state != stateList {
		t.Fatalf("state after enter = %v err=%q", mm.state, mm.form.err)
	}
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("create failed: %s", r.subtitle)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feature-claude")); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
	if !mm.showEmpty {
		t.Fatal("expected stub cards to be shown so the new claude worktree is visible")
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no tab was opened: %v", err)
	}
	if !strings.Contains(string(data), "claude") {
		t.Fatalf("tab command does not launch claude: %q", data)
	}
	if !strings.Contains(string(data), filepath.Join(repo, ".worktrees", "feature-claude")) {
		t.Fatalf("tab did not cd into the new worktree: %q", data)
	}
}
