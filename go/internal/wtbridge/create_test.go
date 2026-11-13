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
	if err := b.Create(repo, "feature-n", "main", "", AgentCursor); err != nil {
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

// TestCreateWithoutWtCodewhale covers the non-cursor launcher on a machine
// with no wt binary: the worktree still lands in the dashboard's scanned
// layout, and no cursor-agent tab is opened.
func TestCreateWithoutWtCodewhale(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("WT_NO_OPEN", "1")
	b := &Bridge{}
	if err := b.Create(repo, "feature-wh", "main", "write the tests", AgentCodewhale); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feature-wh")); err != nil {
		t.Fatalf("worktree dir: %v", err)
	}
}

func TestLaunchCommand(t *testing.T) {
	tests := []struct {
		name   string
		agent  Agent
		prompt string
		want   string
	}{
		{"cursor default", AgentCursor, "", "cursor-agent --yolo"},
		{"cursor prompt", AgentCursor, "fix it", "cursor-agent --yolo -- 'fix it'"},
		{
			"codewhale default", AgentCodewhale, "",
			"codewhale --approval-policy never --sandbox-mode workspace-write --skip-onboarding --fresh",
		},
		{
			"codewhale prompt", AgentCodewhale, "it's broken",
			`codewhale --approval-policy never --sandbox-mode workspace-write --skip-onboarding --fresh -- 'it'\''s broken'`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.agent.LaunchCommand(tc.prompt); got != tc.want {
				t.Fatalf("LaunchCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentNextWraps(t *testing.T) {
	if got := AgentCursor.Next(-1); got != AgentCodewhale {
		t.Fatalf("cursor.Next(-1) = %v, want codewhale", got)
	}
	if got := AgentCodewhale.Next(1); got != AgentCursor {
		t.Fatalf("codewhale.Next(1) = %v, want cursor", got)
	}
	if got := AgentCursor.Next(1); got != AgentCodewhale {
		t.Fatalf("cursor.Next(1) = %v, want codewhale", got)
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
