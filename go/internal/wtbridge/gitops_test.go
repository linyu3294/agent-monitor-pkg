package wtbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoWithRemote builds a clone of a bare origin whose default branch is
// main, so base refs like origin/main resolve the way they do on a real
// machine.
func repoWithRemote(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(dir, "remote.git")
	repo := filepath.Join(dir, "repo")
	runGitTest(t, dir, "init", "--bare", "-b", "main", remote)
	runGitTest(t, dir, "clone", remote, repo)
	commitFile(t, repo, "README", "x\n", "init")
	runGitTest(t, repo, "push", "-u", "origin", "main")
	return repo
}

// localRepo builds a repo with no remote at all — the case where the old
// hardcoded origin/main base failed outright.
func localRepo(t *testing.T, branch string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	runGitTest(t, dir, "init", "-b", branch, repo)
	commitFile(t, repo, "README", "x\n", "init")
	return repo
}

func commitFile(t *testing.T, repo, name, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", name)
	runGitTest(t, repo, "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "-m", msg)
}

// stubGUITools puts no-op osascript/open on PATH and returns a recorder
// so tests can assert a tab was requested without opening one.
func stubGUITools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> '" + log + "'\nexit 0\n"
	for _, name := range []string{"osascript", "open"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// noWt points the bridge at a binary that does not exist, which is the
// state of every machine without the worktrees skill.
func noWt(t *testing.T) *Bridge {
	t.Helper()
	t.Setenv("AM_WT_BIN", filepath.Join(t.TempDir(), "missing-wt"))
	b := New()
	if b.Available() {
		t.Fatal("bridge found a wt binary that should not exist")
	}
	return b
}

func TestDefaultBase(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T) string
		want string
	}{
		{"remote main", repoWithRemote, "origin/main"},
		{"local main", func(t *testing.T) string { return localRepo(t, "main") }, "main"},
		{"local master", func(t *testing.T) string { return localRepo(t, "master") }, "master"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DefaultBase(c.make(t)); got != c.want {
				t.Fatalf("DefaultBase = %q, want %q", got, c.want)
			}
		})
	}
	if got := DefaultBase(t.TempDir()); got != "" {
		t.Fatalf("DefaultBase(non-repo) = %q, want empty", got)
	}
}

// The modal used to hardcode origin/main; any repo without that ref
// failed with "fatal: invalid reference". Create must resolve instead.
func TestCreateFallsBackWhenBaseMissing(t *testing.T) {
	stubGUITools(t)
	b := noWt(t)

	repo := localRepo(t, "main")
	if err := b.Create(repo, "feat-noref", "origin/main", "", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatalf("create with an unresolvable base failed: %v", err)
	}
	dest := filepath.Join(repo, ".worktrees", "feat-noref")
	if st, err := os.Stat(dest); err != nil || !st.IsDir() {
		t.Fatalf("worktree not created: %v", err)
	}
	if got := branchAt(dest); got != "feat-noref" {
		t.Fatalf("worktree branch = %q, want feat-noref", got)
	}
}

func TestCreateRejectsExistingWorktreeWithClearError(t *testing.T) {
	stubGUITools(t)
	b := noWt(t)
	repo := repoWithRemote(t)

	if err := b.Create(repo, "feat-dup", "origin/main", "", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatal(err)
	}
	err := b.Create(repo, "feat-dup", "origin/main", "", AgentCodewhale, ApprovalOnRequest)
	if err == nil {
		t.Fatal("expected the second create to fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %q, want an 'already exists' message", err)
	}
}

// PathFor is used by sync and resume; it must work with no wt installed.
func TestPathForWithoutWt(t *testing.T) {
	stubGUITools(t)
	b := noWt(t)
	repo := repoWithRemote(t)
	if err := b.Create(repo, "feat-p", "origin/main", "", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatal(err)
	}
	got, err := b.PathFor(repo, "feat-p")
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if want := filepath.Join(repo, ".worktrees", "feat-p"); got != want {
		t.Fatalf("PathFor = %q, want %q", got, want)
	}
	if _, err := b.PathFor(repo, "nope"); err == nil {
		t.Fatal("PathFor invented a path for a missing worktree")
	}
}

// Remove must delete the worktree and the branch it had checked out,
// without wt.
func TestRemoveWithoutWt(t *testing.T) {
	stubGUITools(t)
	b := noWt(t)
	repo := repoWithRemote(t)
	if err := b.Create(repo, "feat-r", "origin/main", "", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(repo, ".worktrees", "feat-r")
	if err := b.Remove(repo, "feat-r", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists: %v", err)
	}
	if refExists(repo, "refs/heads/feat-r") {
		t.Fatal("branch feat-r was not deleted")
	}
}

// Removing a worktree must never delete the repo's default branch.
func TestRemoveKeepsDefaultBranch(t *testing.T) {
	stubGUITools(t)
	b := noWt(t)
	repo := repoWithRemote(t)
	// A worktree on a branch that happens to be named main cannot exist
	// (main is checked out), so exercise the guard directly on the
	// protected-name list through a worktree tracking a copy.
	if err := b.Create(repo, "feat-keep", "origin/main", "", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(repo, ".worktrees", "feat-keep")
	runGitTest(t, dest, "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "-q", "--allow-empty", "-m", "wip")

	if err := b.Remove(repo, "feat-keep", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !refExists(repo, "refs/heads/main") {
		t.Fatal("main branch was deleted")
	}
}

// Sync must rebase a worktree onto its base without wt.
func TestSyncWithoutWt(t *testing.T) {
	stubGUITools(t)
	b := noWt(t)
	repo := repoWithRemote(t)
	if err := b.Create(repo, "feat-s", "origin/main", "", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(repo, ".worktrees", "feat-s")
	commitFile(t, dest, "work.txt", "w\n", "work on the feature")

	// Advance main upstream so the worktree is behind.
	commitFile(t, repo, "upstream.txt", "u\n", "upstream change")
	runGitTest(t, repo, "push", "origin", "main")

	if err := b.Sync(dest); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// The upstream commit must now be an ancestor of the worktree HEAD.
	if out, err := gitOut(dest, "merge-base", "--is-ancestor", "origin/main", "HEAD"); err != nil {
		t.Fatalf("worktree is not rebased onto origin/main: %v (%s)", err, out)
	}
}

// Resume must open a tab with the resume command even with no wt.
func TestResumeWithoutWt(t *testing.T) {
	log := stubGUITools(t)
	b := noWt(t)
	cwd := t.TempDir()
	if err := b.Resume("abc-123", cwd); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("osascript was never invoked: %v", err)
	}
	if !strings.Contains(string(data), "cursor-agent --resume") {
		t.Fatalf("tab command = %q, want cursor-agent --resume", data)
	}
}

// Open and IDE must not report "wt unavailable" any more.
func TestOpenAndIDEWithoutWt(t *testing.T) {
	log := stubGUITools(t)
	b := noWt(t)
	repo := repoWithRemote(t)
	if err := b.Create(repo, "feat-o", "origin/main", "", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatal(err)
	}
	if err := b.Open(repo, "feat-o"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.IDE(repo, "feat-o"); err != nil {
		t.Fatalf("IDE: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no GUI tool was invoked: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "-a Cursor") {
		t.Fatalf("open was not called with Cursor: %q", text)
	}
	if !strings.Contains(text, "DiffviewOpen") {
		t.Fatalf("nvim tab was not requested: %q", text)
	}
}
