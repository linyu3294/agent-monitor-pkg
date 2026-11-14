package wtbridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if err := b.Create(repo, "feature-n", "main", "", AgentCursor, ApprovalOnRequest); err != nil {
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
	if err := b.Create(repo, "feature-wh", "main", "write the tests", AgentCodewhale, ApprovalOnRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feature-wh")); err != nil {
		t.Fatalf("worktree dir: %v", err)
	}
}

func TestLaunchCommand(t *testing.T) {
	tests := []struct {
		name     string
		agent    Agent
		approval Approval
		prompt   string
		want     string
	}{
		{"cursor asks by default", AgentCursor, ApprovalOnRequest, "", "cursor-agent"},
		{"cursor unattended only when chosen", AgentCursor, ApprovalNever, "", "cursor-agent --yolo"},
		{"cursor prompt", AgentCursor, ApprovalOnRequest, "fix it", "cursor-agent -- 'fix it'"},
		{
			"codewhale asks by default", AgentCodewhale, ApprovalOnRequest, "",
			"codewhale --approval-policy on-request --sandbox-mode workspace-write --skip-onboarding --fresh",
		},
		{
			"codewhale unattended only when chosen", AgentCodewhale, ApprovalNever, "",
			"codewhale --approval-policy never --sandbox-mode workspace-write --skip-onboarding --fresh",
		},
		{
			"codewhale strict", AgentCodewhale, ApprovalUntrusted, "",
			"codewhale --approval-policy untrusted --sandbox-mode workspace-write --skip-onboarding --fresh",
		},
		{
			"codewhale prompt", AgentCodewhale, ApprovalNever, "it's broken",
			`codewhale --approval-policy never --sandbox-mode workspace-write --skip-onboarding --fresh -- 'it'\''s broken'`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.agent.LaunchCommand(tc.prompt, tc.approval); got != tc.want {
				t.Fatalf("LaunchCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

// A zero-valued Approval must ask the user, never run unattended: the
// dangerous posture has to be an explicit opt-in.
func TestApprovalZeroValueAsksUser(t *testing.T) {
	var zero Approval
	if zero != ApprovalOnRequest {
		t.Fatalf("zero Approval = %v, want on-request", zero)
	}
	if got := zero.Flag(); got != "on-request" {
		t.Fatalf("Flag = %q, want on-request", got)
	}
	if got := AgentCodewhale.LaunchCommand("", zero); strings.Contains(got, "--yolo") || strings.Contains(got, "--approval-policy never") {
		t.Fatalf("zero-valued approval launched unattended: %q", got)
	}
}

func TestApprovalNextCycles(t *testing.T) {
	if got := ApprovalOnRequest.Next(1); got != ApprovalNever {
		t.Fatalf("on-request.Next(1) = %v, want never", got)
	}
	if got := ApprovalNever.Next(1); got != ApprovalUntrusted {
		t.Fatalf("never.Next(1) = %v, want untrusted", got)
	}
	if got := ApprovalUntrusted.Next(1); got != ApprovalOnRequest {
		t.Fatalf("untrusted.Next(1) = %v, want on-request", got)
	}
	if got := ApprovalOnRequest.Next(-1); got != ApprovalUntrusted {
		t.Fatalf("on-request.Next(-1) = %v, want untrusted", got)
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
