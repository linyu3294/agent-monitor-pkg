package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// writeClaudeTranscript writes a transcript in Claude Code's JSONL shape:
// one record per event, with the workspace stated on every record.
// Claude's stored directory name is irrelevant here on purpose — the
// scanner reads the workspace from the records, not from the path.
func writeClaudeTranscript(t *testing.T, dir, id string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newClaudeScanner(t *testing.T, projectsDir, claudeDir string) *Scanner {
	t.Helper()
	return New(Options{
		ProjectsDir:  projectsDir,
		CodewhaleDir: filepath.Join(t.TempDir(), "no-codewhale"),
		ClaudeDir:    claudeDir,
		// Isolate from the real Cursor/Claude stores on the host.
		IdleThreshold: 20 * time.Minute,
		MaxCards:      50,
	})
}

func claudeProjectDir(t *testing.T, claudeDir, key string) string {
	t.Helper()
	dir := filepath.Join(claudeDir, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestScanIncludesClaudeSessions(t *testing.T) {
	projects := t.TempDir()
	claudeDir := t.TempDir()
	_, worktree := makeWorktreeRepo(t, "feat-x")

	const id = "8b7c6d5e-4f3a-4b2c-9d1e-0a1b2c3d4e5f"
	writeClaudeTranscript(t, claudeProjectDir(t, claudeDir, "-tmp-repo-worktrees-feat-x"), id,
		`{"type":"user","cwd":"`+worktree+`","sessionId":"`+id+`","message":{"role":"user","content":"wire up the cards"}}`,
		`{"type":"assistant","cwd":"`+worktree+`","sessionId":"`+id+`","message":{"role":"assistant","content":[{"type":"text","text":"on it"}]}}`,
		`{"type":"assistant","cwd":"`+worktree+`","sessionId":"`+id+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}`,
		// A subagent record finishes after the main thread's tool call.
		// It must not become the card's last turn.
		`{"type":"assistant","isSidechain":true,"cwd":"`+worktree+`","message":{"role":"assistant","content":[{"type":"text","text":"subagent done"}]}}`,
		`{"type":"summary","summary":"wired up the cards"}`,
	)
	// Not sessions: subagent transcripts live in a directory and the
	// store also holds index files.
	if err := os.MkdirAll(filepath.Join(claudeDir, "-tmp-repo-worktrees-feat-x", id, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "-tmp-repo-worktrees-feat-x", "sessions-index.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := newClaudeScanner(t, projects, claudeDir).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1: %+v", len(sessions), sessions)
	}
	got := sessions[0]
	if got.Source != session.SourceClaude {
		t.Errorf("Source = %q, want %q", got.Source, session.SourceClaude)
	}
	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	if got.CWDHint != worktree {
		t.Errorf("CWDHint = %q, want %q", got.CWDHint, worktree)
	}
	wantDir := EncodeForCursor(strings.TrimPrefix(worktree, "/"))
	if got.ProjectDir != wantDir {
		t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, wantDir)
	}
	if got.WorktreeSlug != "feat-x" {
		t.Errorf("WorktreeSlug = %q, want feat-x", got.WorktreeSlug)
	}
	if got.LastRole != session.RoleAssistant {
		t.Errorf("LastRole = %q, want assistant", got.LastRole)
	}
	if got.LastTool != "Bash" {
		t.Errorf("LastTool = %q, want Bash", got.LastTool)
	}
	if got.LastText != "" {
		t.Errorf("LastText = %q, want empty (a subagent's text is not the agent's last words)", got.LastText)
	}
	if got.AgentLabel() != "claude" {
		t.Errorf("AgentLabel = %q, want claude", got.AgentLabel())
	}
	if got.ResumeCommand() != "claude --resume "+id {
		t.Errorf("ResumeCommand = %q", got.ResumeCommand())
	}
}

func TestScanSkipsStaleClaudeSessions(t *testing.T) {
	claudeDir := t.TempDir()
	cwd := t.TempDir()
	dir := claudeProjectDir(t, claudeDir, "-tmp-plain-repo")

	old := writeClaudeTranscript(t, dir, "aaaaaaaa-0000-0000-0000-000000000000",
		`{"type":"user","cwd":"`+cwd+`","message":{"role":"user","content":"hello"}}`)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	writeClaudeTranscript(t, dir, "bbbbbbbb-0000-0000-0000-000000000000",
		`{"type":"user","cwd":"`+cwd+`","message":{"role":"user","content":"second"}}`)

	sessions := newClaudeScanner(t, t.TempDir(), claudeDir).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != "bbbbbbbb-0000-0000-0000-000000000000" {
		t.Fatalf("kept %q, want the fresh session", sessions[0].ID)
	}
}

// A worktree card must survive the idle cutoff: it is the only handle the
// dashboard has for resuming or removing that worktree.
func TestClaudeWorktreeCardSurvivesIdleCutoff(t *testing.T) {
	_, worktree := makeWorktreeRepo(t, "feat-cards")
	claudeDir := t.TempDir()
	dir := claudeProjectDir(t, claudeDir, "-tmp-repo-worktrees-feat-cards")

	path := writeClaudeTranscript(t, dir, "eeeeeeee-0000-0000-0000-000000000000",
		`{"type":"user","cwd":"`+worktree+`","message":{"role":"user","content":"go"}}`)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	sessions := newClaudeScanner(t, t.TempDir(), claudeDir).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].WorktreeSlug != "feat-cards" {
		t.Fatalf("WorktreeSlug = %q, want feat-cards", sessions[0].WorktreeSlug)
	}
}

// The ghost-card guard: once the worktree directory is gone, the idle
// cutoff takes the card with it.
func TestClaudeOrphanedWorktreeAgesOut(t *testing.T) {
	_, worktree := makeWorktreeRepo(t, "feat-gone")
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	claudeDir := t.TempDir()
	dir := claudeProjectDir(t, claudeDir, "-tmp-repo-worktrees-feat-gone")

	path := writeClaudeTranscript(t, dir, "ffffffff-0000-0000-0000-000000000000",
		`{"type":"user","cwd":"`+worktree+`","message":{"role":"user","content":"go"}}`)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	if sessions := newClaudeScanner(t, t.TempDir(), claudeDir).Scan(); len(sessions) != 0 {
		t.Fatalf("orphaned worktree kept %d sessions, want 0", len(sessions))
	}
}

// A transcript whose workspace cannot be established still yields a card,
// labelled honestly rather than guessed.
func TestClaudeUnknownWorkspace(t *testing.T) {
	claudeDir := t.TempDir()
	dir := claudeProjectDir(t, claudeDir, "-tmp-gone")
	writeClaudeTranscript(t, dir, "dddddddd-0000-0000-0000-000000000000",
		`{"type":"user","cwd":"/nonexistent/workspace/for-test","message":{"role":"user","content":"hi"}}`)

	sessions := newClaudeScanner(t, t.TempDir(), claudeDir).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].Project != "(unknown workspace)" {
		t.Fatalf("Project = %q, want the unknown-workspace label", sessions[0].Project)
	}
	if sessions[0].ProjectDir != "" || sessions[0].WorktreeSlug != "" {
		t.Fatalf("unknown workspace invented a project dir: %+v", sessions[0])
	}
}

// The mtime cache must serve a quiescent transcript without re-parsing.
func TestScanClaudeMtimeCache(t *testing.T) {
	claudeDir := t.TempDir()
	cwd := t.TempDir()
	dir := claudeProjectDir(t, claudeDir, "-tmp-plain-repo")
	writeClaudeTranscript(t, dir, "cccccccc-0000-0000-0000-000000000000",
		`{"type":"user","cwd":"`+cwd+`","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","cwd":"`+cwd+`","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}`,
	)

	scn := newClaudeScanner(t, t.TempDir(), claudeDir)
	first := scn.Scan()
	if len(first) != 1 || first[0].LastText != "first reply" {
		t.Fatalf("first scan = %+v", first)
	}
	second := scn.Scan()
	if len(second) != 1 || second[0].LastText != "first reply" {
		t.Fatalf("cached scan = %+v", second)
	}
}

// Claude's record kinds, exercised directly: only conversation turns are
// answers, sidechain records belong to subagents, and a user prompt is
// not the assistant's last words.
func TestClaudeLastTurnShapes(t *testing.T) {
	cases := []struct {
		name string
		line string
		role session.Role
		tool string
		text string
	}{
		{
			name: "assistant text",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"here is the fix"}]}}`,
			role: session.RoleAssistant,
			text: "here is the fix",
		},
		{
			name: "assistant tool call",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"x"}}]}}`,
			role: session.RoleAssistant,
			tool: "Edit",
		},
		{
			name: "user prompt is not card text",
			line: `{"type":"user","message":{"role":"user","content":"please fix it"}}`,
			role: session.RoleUser,
		},
		{
			name: "tool result is a user turn",
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
			role: session.RoleUser,
		},
		{
			name: "sidechain is skipped",
			line: `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"sub"}]}}`,
			role: session.RoleUnknown,
		},
		{
			name: "summary is not a turn",
			line: `{"type":"summary","summary":"did the thing"}`,
			role: session.RoleUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, tool, text := claudeLastTurn([]byte(c.line))
			if role != c.role || tool != c.tool || text != c.text {
				t.Fatalf("claudeLastTurn = (%q,%q,%q), want (%q,%q,%q)", role, tool, text, c.role, c.tool, c.text)
			}
		})
	}
}
