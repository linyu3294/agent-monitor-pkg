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
)

// cursorWorktreeSession builds a Cursor-shaped session for a real
// worktree: a transcript on disk at the path Cursor uses, the encoded
// project dir, and a CWD hint only when the transcript states one.
func cursorWorktreeSession(t *testing.T, slug string, withCWDHint bool) (primary string, sess session.Session) {
	return cursorWorktreeSessionWithHint(t, slug, func(p string) string {
		if !withCWDHint {
			return ""
		}
		return filepath.Join(p, ".worktrees", slug)
	})
}

// cursorWorktreeSessionWithHint builds the same session but lets the
// caller decide what the transcript's cwd observation says.
func cursorWorktreeSessionWithHint(t *testing.T, slug string, hint func(primary string) string) (primary string, sess session.Session) {
	t.Helper()
	primary, _ = tuiRepoWithWorktree(t, slug)
	id := "11111111-2222-3333-4444-555555555555"

	encodedPrimary := scanner.EncodeForCursor(strings.TrimPrefix(primary, "/"))
	encodedSlug := scanner.EncodeForCursor(slug)
	projectDir := encodedPrimary + "-worktrees-" + encodedSlug

	transcriptDir := filepath.Join(t.TempDir(), "projects", projectDir, "agent-transcripts", id)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(transcriptDir, id+".jsonl")
	body := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell"}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	sess = session.Session{
		ID:             id,
		Source:         session.SourceCursor,
		ProjectDir:     projectDir,
		Project:        "repo",
		WorktreeSlug:   encodedSlug,
		TranscriptPath: transcript,
		Mtime:          time.Now().Unix(),
		LastRole:       session.RoleAssistant,
		LastTool:       "Shell",
		State:          session.StateActive,
	}
	if hint != nil {
		sess.CWDHint = hint(primary)
	}
	return primary, sess
}

// Every filesystem-touching action must resolve the same worktree for a
// Cursor card as it does for a Codewhale one — both with and without a
// CWD hint, since Cursor transcripts often omit one.
func TestCursorResolvesWorktree(t *testing.T) {
	for _, withHint := range []bool{true, false} {
		name := "with cwd hint"
		if !withHint {
			name = "without cwd hint"
		}
		t.Run(name, func(t *testing.T) {
			guiStubPATH(t)
			primary, sess := cursorWorktreeSession(t, "feat-cursor", withHint)
			m := newTestModel(sess)

			want := filepath.Join(primary, ".worktrees", "feat-cursor")
			if got := m.resolveRepoPath(&sess); got != want {
				t.Fatalf("resolveRepoPath = %q, want %q", got, want)
			}
			if got := sessionPrimary(&sess); got != primary {
				t.Fatalf("sessionPrimary = %q, want %q", got, primary)
			}
		})
	}
}

// `a` on a Cursor card must open cursor-agent --resume in the worktree.
func TestCursorAttach(t *testing.T) {
	log := guiStubPATH(t)
	primary, sess := cursorWorktreeSession(t, "feat-cursor-a", true)

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
	if !strings.Contains(text, "cursor-agent --resume") || !strings.Contains(text, sess.ID) {
		t.Fatalf("tab command = %q, want cursor-agent --resume <id>", text)
	}
	if !strings.Contains(text, filepath.Join(primary, ".worktrees", "feat-cursor-a")) {
		t.Fatalf("tab did not cd into the worktree: %q", text)
	}
}

// `s` on a Cursor card must rebase that worktree.
func TestCursorSync(t *testing.T) {
	guiStubPATH(t)
	primary, sess := cursorWorktreeSession(t, "feat-cursor-s", true)
	dest := filepath.Join(primary, ".worktrees", "feat-cursor-s")

	if err := os.WriteFile(filepath.Join(dest, "w.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dest, "add", "w.txt")
	gitTest(t, dest, "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "-m", "work")

	_, cmd := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	res := results(cmd)
	if len(res) == 0 {
		t.Fatal("sync produced no result")
	}
	for _, r := range res {
		if r.failed {
			t.Fatalf("sync failed: %s", r.subtitle)
		}
	}
}

// `r` on a Cursor worktree card must remove the worktree and branch.
func TestCursorRemove(t *testing.T) {
	guiStubPATH(t)
	primary, sess := cursorWorktreeSession(t, "feat-cursor-r", true)

	_, cmd := drive(t, newTestModel(sess),
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}},
	)
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("remove failed: %s", r.subtitle)
		}
	}
	if _, err := os.Stat(filepath.Join(primary, ".worktrees", "feat-cursor-r")); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
}

// `o` on a Cursor worktree card opens the worktree in Cursor.
func TestCursorOpen(t *testing.T) {
	log := guiStubPATH(t)
	primary, sess := cursorWorktreeSession(t, "feat-cursor-o", true)

	_, cmd := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("open failed: %s", r.subtitle)
		}
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "-a Cursor") {
		t.Fatalf("open was not called with Cursor: %q", data)
	}
	if !strings.Contains(string(data), filepath.Join(primary, ".worktrees", "feat-cursor-o")) {
		t.Fatalf("open used the wrong path: %q", data)
	}
}

// `i` on a Cursor worktree card opens nvim+Diffview there.
func TestCursorIDE(t *testing.T) {
	log := guiStubPATH(t)
	_, sess := cursorWorktreeSession(t, "feat-cursor-i", true)

	_, cmd := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("IDE failed: %s", r.subtitle)
		}
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "DiffviewOpen") {
		t.Fatalf("nvim tab was not requested: %q", data)
	}
}

// `f` is Cursor-only and must actually attempt the follow tab, not
// redirect to 'a' the way a Codewhale card does.
func TestCursorFollow(t *testing.T) {
	// Stub the GUI tools plus `am`, which follow resolves as a launcher.
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

	_, sess := cursorWorktreeSession(t, "feat-cursor-f", true)
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

// `n` from a Cursor card must prefill that card's repo and create the
// worktree there, so the modal is usable without retyping a path.
func TestCursorNewWorktreePrefillsRepo(t *testing.T) {
	stubDir := t.TempDir()
	for _, name := range []string{"codewhale", "osascript"} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))

	primary, sess := cursorWorktreeSession(t, "feat-cursor-n", true)
	mdl, _ := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if mdl.state != stateNewWt {
		t.Fatalf("state = %v, want the new-worktree modal", mdl.state)
	}
	if got := mdl.form.repo.Value(); got != primary {
		t.Fatalf("repo prefill = %q, want %q", got, primary)
	}
	if got := mdl.form.base.Value(); got == "" {
		t.Fatal("base was not detected from the repo")
	}

	mdl.form.branch.SetValue("feature-from-cursor")
	mdl, cmd := drive(t, mdl, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("expected a create command, form err=%q", mdl.form.err)
	}
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("create failed: %s", r.subtitle)
		}
	}
	if _, err := os.Stat(filepath.Join(primary, ".worktrees", "feature-from-cursor")); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
}

// `x` on a Cursor card with no live process must say so plainly rather
// than reporting a confusing failure.
func TestCursorPauseWithoutLiveProcess(t *testing.T) {
	guiStubPATH(t)
	_, sess := cursorWorktreeSession(t, "feat-cursor-x", true)

	mdl, _ := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !strings.Contains(mdl.confirm.title, "no live agent process") {
		t.Fatalf("confirm = %q, want a clear 'no live agent process' message", mdl.confirm.title)
	}
}

// `y` copies the chat id regardless of source.
func TestCursorCopyID(t *testing.T) {
	guiStubPATH(t)
	_, sess := cursorWorktreeSession(t, "feat-cursor-y", true)

	mdl, _ := drive(t, newTestModel(sess), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !strings.Contains(mdl.confirm.subtitle, sess.ID) {
		t.Fatalf("confirm = %q, want the copied id %q", mdl.confirm.subtitle, sess.ID)
	}
}

// A real card's transcript recorded a `cd` into the cursor-agent install
// directory, not the workspace. Trusting that observation made removal
// look for `.worktrees/<slug>` inside
// ~/.local/share/cursor-agent/versions/<v> and fail. For a worktree
// session the encoded project dir is authoritative; a shell cwd is only
// a hint.
func TestCursorResolutionIgnoresMisleadingCWDHint(t *testing.T) {
	guiStubPATH(t)
	// Stand in for the agent's own install directory: a real directory
	// that exists but is not the workspace and is not a git repo.
	installDir := filepath.Join(t.TempDir(), "cursor-agent", "versions", "2026.11.05-86047df")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}

	primary, sess := cursorWorktreeSessionWithHint(t, "feat-Bionic", func(string) string {
		return installDir
	})

	wantWT := filepath.Join(primary, ".worktrees", "feat-Bionic")
	if got := sessionWorkspace(&sess); got != wantWT {
		t.Fatalf("sessionWorkspace = %q, want the worktree %q", got, wantWT)
	}
	if got := sessionPrimary(&sess); got != primary {
		t.Fatalf("sessionPrimary = %q, want %q", got, primary)
	}
	m := newTestModel(sess)
	if got := m.resolveRepoPath(&sess); got != wantWT {
		t.Fatalf("resolveRepoPath = %q, want %q", got, wantWT)
	}

	// Removal must act on the worktree, not the install directory.
	_, cmd := drive(t, m,
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}},
	)
	for _, r := range results(cmd) {
		if r.failed {
			t.Fatalf("remove failed: %s", r.subtitle)
		}
	}
	if _, err := os.Stat(wantWT); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
	if _, err := os.Stat(installDir); err != nil {
		t.Fatalf("the install directory was disturbed: %v", err)
	}
}

// A session whose workspace is not a repo at all must not report a
// primary checkout — callers treat that value as a base for
// `.worktrees/` lookups, so a wrong answer sends them elsewhere.
func TestNonRepoWorkspaceHasNoPrimary(t *testing.T) {
	plain := t.TempDir()
	sess := session.Session{
		ID:         "x",
		Source:     session.SourceCursor,
		ProjectDir: scanner.EncodeForCursor(strings.TrimPrefix(plain, "/")),
		CWDHint:    plain,
	}
	if got := sessionPrimary(&sess); got != "" {
		t.Fatalf("sessionPrimary = %q, want empty for a non-repo workspace", got)
	}
	if got := sessionWorkspace(&sess); got != plain {
		t.Fatalf("sessionWorkspace = %q, want %q", got, plain)
	}
}

// A primary card whose transcript recorded a cd into the agent's own
// install tree must fall back to the encoded project dir, not resume
// into the binary directory.
func TestPrimarySessionIgnoresAgentRuntimeHint(t *testing.T) {
	primary, _ := tuiRepoWithWorktree(t, "unused")
	installDir := filepath.Join(t.TempDir(), ".local", "share", "cursor-agent", "versions", "2026.11.05-86047df")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sess := session.Session{
		ID:         "x",
		Source:     session.SourceCursor,
		ProjectDir: scanner.EncodeForCursor(strings.TrimPrefix(primary, "/")),
		CWDHint:    installDir,
	}
	if got := sessionWorkspace(&sess); got != primary {
		t.Fatalf("sessionWorkspace = %q, want the repo %q", got, primary)
	}
	if got := sessionPrimary(&sess); got != primary {
		t.Fatalf("sessionPrimary = %q, want %q", got, primary)
	}
}
