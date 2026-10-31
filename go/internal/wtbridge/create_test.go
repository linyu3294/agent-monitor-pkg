package wtbridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExpandUserPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got := expandUserPath("~/Documents/credit-platform/")
	want := filepath.Join(home, "Documents", "credit-platform")
	if got != want {
		t.Fatalf("expandUserPath = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err == nil {
		if primary := GitPrimary("~/Documents/credit-platform/"); primary == "" {
			t.Fatal("GitPrimary rejected ~/Documents/credit-platform/")
		}
	}
}

func TestCreateWithoutWt(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("WT_NO_OPEN", "1")
	b := &Bridge{}
	if err := b.Create(repo, "feature-n", "main", ""); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(repo, ".worktrees", "feature-n")
	if st, err := os.Stat(dest); err != nil || !st.IsDir() {
		t.Fatalf("worktree dir: %v", err)
	}
	if got := GitPrimary(dest); got != repo {
		t.Fatalf("GitPrimary(worktree) = %q, want %q", got, repo)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, dir, "add", "README")
	runGitTest(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=test", "commit", "-m", "init")
	return dir
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
