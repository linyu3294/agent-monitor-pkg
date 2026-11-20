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

// wt create --iterm can only express --yolo, so routing the auto posture
// through it would silently launch something the user did not choose.
// Create must fall back to the direct git path instead.
func TestCreateBypassesWtForAutoApproval(t *testing.T) {
	repo := initRepo(t)
	marker := filepath.Join(t.TempDir(), "wt-was-called")
	stub := filepath.Join(t.TempDir(), "wt")
	script := "#!/bin/sh\ntouch '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AM_WT_BIN", stub)
	t.Setenv("WT_NO_OPEN", "1")

	b := New()
	if !b.Available() {
		t.Fatal("stub wt was not detected")
	}
	if err := b.Create(repo, "feature-auto", "main", "", AgentCursor, ApprovalAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("wt was invoked for the auto posture, which it cannot express")
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feature-auto")); err != nil {
		t.Fatalf("worktree not created: %v", err)
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
		{"cursor auto uses smart auto", AgentCursor, ApprovalAuto, "", "cursor-agent --auto-review"},
		{"cursor unattended only when chosen", AgentCursor, ApprovalNever, "", "cursor-agent --yolo"},
		{"cursor untrusted maps to its default", AgentCursor, ApprovalUntrusted, "", "cursor-agent"},
		{"cursor prompt", AgentCursor, ApprovalOnRequest, "fix it", "cursor-agent -- 'fix it'"},
		{
			"codewhale leaves the permission dial free", AgentCodewhale, ApprovalOnRequest, "",
			"codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh",
		},
		{
			"codewhale auto does not pin the guardian", AgentCodewhale, ApprovalAuto, "",
			"codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh",
		},
		{
			"codewhale never does not pin the dial", AgentCodewhale, ApprovalNever, "",
			"codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh",
		},
		{
			"codewhale untrusted does not pin the dial", AgentCodewhale, ApprovalUntrusted, "",
			"codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh",
		},
		{
			"codewhale prompt", AgentCodewhale, ApprovalNever, "it's broken",
			`codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh -- 'it'\''s broken'`,
		},
		// Claude Code 2.1.x defaults to auto mode, so on-request must be
		// pinned explicitly or the label would promise prompts it does not
		// produce.
		{
			"claude asks by default", AgentClaude, ApprovalOnRequest, "",
			"claude --permission-mode manual",
		},
		{
			"claude auto keeps the guardian", AgentClaude, ApprovalAuto, "",
			"claude --permission-mode auto",
		},
		{
			"claude never skips permissions", AgentClaude, ApprovalNever, "",
			"claude --dangerously-skip-permissions",
		},
		{
			"claude untrusted denies unapproved calls", AgentClaude, ApprovalUntrusted, "",
			"claude --permission-mode dontAsk",
		},
		{
			"claude prompt", AgentClaude, ApprovalOnRequest, "fix it",
			`claude --permission-mode manual -- 'fix it'`,
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
	// Least to most permissive, then the strictest.
	want := []Approval{ApprovalOnRequest, ApprovalAuto, ApprovalNever, ApprovalUntrusted}
	got := ApprovalOnRequest
	for i, w := range want {
		if got != w {
			t.Fatalf("step %d = %v, want %v", i, got, w)
		}
		got = got.Next(1)
	}
	if got != ApprovalOnRequest {
		t.Fatalf("cycle did not wrap: %v", got)
	}
	if back := ApprovalOnRequest.Next(-1); back != ApprovalUntrusted {
		t.Fatalf("on-request.Next(-1) = %v, want untrusted", back)
	}
}

// Auto must be a real distinct posture, not a relabelled one, and it
// must use a value the CLI actually accepts. `on-failure` belongs to the
// `sandbox check --ask` enum, not `--approval-policy`; launching with it
// fails immediately.
func TestApprovalAutoIsDistinct(t *testing.T) {
	if ApprovalAuto.Flag() == ApprovalOnRequest.Flag() {
		t.Fatal("auto and on-request share a codewhale flag")
	}
	if got := AgentCursor.LaunchCommand("", ApprovalAuto); !strings.Contains(got, "--auto-review") {
		t.Fatalf("cursor auto = %q, want --auto-review", got)
	}
	got := AgentCodewhale.LaunchCommand("", ApprovalAuto)
	if strings.Contains(got, "--approval-policy") {
		t.Fatalf("codewhale launch pins the permission dial: %q", got)
	}
	if !strings.Contains(got, "--sandbox-mode danger-full-access") {
		t.Fatalf("codewhale launch = %q, want the sandbox off", got)
	}
	if got := ApprovalAuto.Flag(); got != "auto" {
		t.Fatalf("ApprovalAuto.Flag() = %q, want auto", got)
	}
}

func TestAgentNextWraps(t *testing.T) {
	// The modal's order is cursor-agent → claude → codewhale.
	if got := AgentCursor.Next(1); got != AgentClaude {
		t.Fatalf("cursor.Next(1) = %v, want claude", got)
	}
	if got := AgentClaude.Next(1); got != AgentCodewhale {
		t.Fatalf("claude.Next(1) = %v, want codewhale", got)
	}
	if got := AgentCodewhale.Next(1); got != AgentCursor {
		t.Fatalf("codewhale.Next(1) = %v, want cursor", got)
	}
	if got := AgentCursor.Next(-1); got != AgentCodewhale {
		t.Fatalf("cursor.Next(-1) = %v, want codewhale", got)
	}
	if got := AgentClaude.Next(-1); got != AgentCursor {
		t.Fatalf("claude.Next(-1) = %v, want cursor", got)
	}
}

// TestClaudePermissionModesAcceptedByCLI validates the launch flags
// against the installed binary instead of against our own assumptions.
//
// Claude Code validates --permission-mode before it does anything else, so
// a prompt with an invalid mode fails with the accepted enum and never
// reaches the API. That makes this a safe, offline check.
//
// The default row is the one that matters most: 2.1.x starts in `auto`, so
// a launch that forgot the flag would silently stop asking the human.
// Skipped when claude is not installed.
func TestClaudePermissionModesAcceptedByCLI(t *testing.T) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}
	for _, a := range approvals {
		a := a
		t.Run(a.String(), func(t *testing.T) {
			// No prompt, so the launch line contains no quoted values
			// and Fields recovers the exact argv.
			launch := strings.Fields(AgentClaude.LaunchCommand("", a))
			args := append([]string{"-p", "reply with one word"}, launch[1:]...)
			probe := exec.Command(bin, args...)
			probe.Dir = t.TempDir()
			out, err := probe.CombinedOutput()
			if strings.Contains(string(out), "is invalid") {
				t.Fatalf("claude rejects the launch %v:\n%s", launch, out)
			}
			if strings.Contains(string(out), "Not logged in") {
				t.Skip("claude is not authenticated; flag validation needs a login")
			}
			if err != nil {
				t.Fatalf("claude launch %v failed: %v\n%s", launch, err, out)
			}
		})
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

// TestApprovalFlagsAcceptedByCodewhale validates the flags we generate
// against the installed CLI instead of against our own assumptions.
//
// `codewhale --approval-policy <v> doctor` is a read-only command that
// runs the same enum validation the launch path does, so it is a safe
// way to catch a wrong value without spawning an agent. Skipped when
// codewhale is not installed.
//
// This test exists because the auto posture was once mapped to
// `on-failure` — a value from the unrelated `sandbox check --ask` enum.
// `sessions list` accepted it (it never validates the global option),
// so the mistake survived until a real launch failed.
func TestApprovalFlagsAcceptedByCodewhale(t *testing.T) {
	bin, err := exec.LookPath("codewhale")
	if err != nil {
		t.Skip("codewhale not on PATH")
	}
	for _, a := range approvals {
		a := a
		t.Run(a.String(), func(t *testing.T) {
			cmd := exec.Command(bin, "--approval-policy", a.Flag(), "doctor")
			out, err := cmd.CombinedOutput()
			if err != nil && strings.Contains(string(out), "Invalid approval_policy") {
				t.Fatalf("codewhale rejects --approval-policy %q:\n%s", a.Flag(), out)
			}
		})
	}
}

// Codewhale's permission dial is Shift+Tab inside the session. Pinning
// --approval-policy locks that dial, so the Auto-Review guardian cannot be
// left. The sandbox flag still precedes the prompt separator.
func TestCodewhaleLaunchFlagOrder(t *testing.T) {
	got := AgentCodewhale.LaunchCommand("do the thing", ApprovalAuto)
	if strings.Contains(got, "--approval-policy") {
		t.Fatalf("launch pins the permission dial: %q", got)
	}
	tokens := strings.Fields(got)
	sandboxAt, sepAt := -1, -1
	for i, tok := range tokens {
		switch tok {
		case "--sandbox-mode":
			sandboxAt = i
		case "--":
			sepAt = i
		}
	}
	if sandboxAt < 0 || sepAt < 0 {
		t.Fatalf("launch command missing a required token: %q", got)
	}
	if sandboxAt > sepAt {
		t.Fatalf("a global option was placed after the prompt separator: %q", got)
	}
	if tokens[sandboxAt+1] != "danger-full-access" {
		t.Fatalf("sandbox token = %q, want danger-full-access", tokens[sandboxAt+1])
	}
}

// The network posture is independent of approval. Codewhale is unsandboxed
// either way; cursor-agent only receives --sandbox disabled when network is on.
func TestLaunchCommandNetwork(t *testing.T) {
	tests := []struct {
		name     string
		agent    Agent
		approval Approval
		network  Network
		want     string
	}{
		{
			"codewhale confined by default", AgentCodewhale, ApprovalOnRequest, NetworkOff,
			"codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh",
		},
		{
			"codewhale egress lifts the sandbox", AgentCodewhale, ApprovalOnRequest, NetworkOn,
			"codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh",
		},
		{
			"network does not pin the permission dial", AgentCodewhale, ApprovalNever, NetworkOn,
			"codewhale --sandbox-mode danger-full-access --skip-onboarding --fresh",
		},
		{
			"cursor confined emits no sandbox flag", AgentCursor, ApprovalOnRequest, NetworkOff,
			"cursor-agent",
		},
		{
			"cursor egress disables its sandbox", AgentCursor, ApprovalOnRequest, NetworkOn,
			"cursor-agent --sandbox disabled",
		},
		{
			"cursor egress with auto keeps both", AgentCursor, ApprovalAuto, NetworkOn,
			"cursor-agent --auto-review --sandbox disabled",
		},
		// Claude Code has no sandbox switch, so `net:` must not appear on
		// its command line at all — the row would be a promise the CLI
		// cannot keep.
		{
			"claude ignores the network row", AgentClaude, ApprovalAuto, NetworkOn,
			"claude --permission-mode auto",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.agent.LaunchCommandWith("", tc.approval, tc.network); got != tc.want {
				t.Fatalf("LaunchCommandWith = %q, want %q", got, tc.want)
			}
		})
	}
}

// LaunchCommand must stay byte-identical to the pre-network behaviour, because
// it is the seam every existing caller and test goes through.
func TestLaunchCommandIsConfinedByDefault(t *testing.T) {
	for _, agent := range []Agent{AgentCodewhale, AgentClaude, AgentCursor} {
		for _, approval := range approvals {
			want := agent.LaunchCommandWith("", approval, NetworkOff)
			if got := agent.LaunchCommand("", approval); got != want {
				t.Fatalf("%v/%v: LaunchCommand = %q, want %q", agent, approval, got, want)
			}
		}
	}
}

// wt can only express --yolo and --prompt. Anything it would silently drop must
// route through git instead, or the user gets a launch they did not choose.
func TestLaunchOptionsWTExpressible(t *testing.T) {
	tests := []struct {
		name string
		opts LaunchOptions
		want bool
	}{
		{"plain cursor launch", LaunchOptions{Agent: AgentCursor, Approval: ApprovalOnRequest}, true},
		{"cursor unattended", LaunchOptions{Agent: AgentCursor, Approval: ApprovalNever}, true},
		{"codewhale never goes through wt", LaunchOptions{Agent: AgentCodewhale}, false},
		{"claude never goes through wt", LaunchOptions{Agent: AgentClaude}, false},
		{"auto posture cannot be expressed", LaunchOptions{Agent: AgentCursor, Approval: ApprovalAuto}, false},
		{"network cannot be expressed", LaunchOptions{Agent: AgentCursor, Network: NetworkOn}, false},
		{"setup command cannot be expressed", LaunchOptions{Agent: AgentCursor, Before: "nvm use"}, false},
		{"blank setup is still expressible", LaunchOptions{Agent: AgentCursor, Before: "   "}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.wtExpressible(); got != tc.want {
				t.Fatalf("wtExpressible = %v, want %v", got, tc.want)
			}
		})
	}
}

// launchLine is what actually gets typed into the new tab. It is composed as one
// shell line, so the shape of that line — cd, setup, agent, in that order, joined
// by && — is worth pinning down without spawning iTerm.
func TestLaunchLine(t *testing.T) {
	tests := []struct {
		name   string
		cwd    string
		before string
		cmd    string
		want   string
	}{
		{
			"no setup command",
			"/tmp/repo", "", "cursor-agent",
			"cd '/tmp/repo' && cursor-agent",
		},
		{
			"blank setup is ignored",
			"/tmp/repo", "   ", "cursor-agent",
			"cd '/tmp/repo' && cursor-agent",
		},
		{
			"setup runs after the cd and before the agent",
			"/tmp/repo", "aws-login bora", "codewhale --approval-policy on-request",
			`cd '/tmp/repo' && if [ -n "$BASH_VERSION" ] && [ -f "$HOME/.bash_profile" ]; then . "$HOME/.bash_profile"; fi && aws-login bora && codewhale --approval-policy on-request`,
		},
		{
			"a failing setup still short-circuits the agent",
			"/tmp/repo", "false", "cursor-agent",
			`cd '/tmp/repo' && if [ -n "$BASH_VERSION" ] && [ -f "$HOME/.bash_profile" ]; then . "$HOME/.bash_profile"; fi && false && cursor-agent`,
		},
		{
			"cwd is quoted",
			"/tmp/my repo", "", "cursor-agent",
			"cd '/tmp/my repo' && cursor-agent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := launchLine(tc.cwd, tc.before, tc.cmd); got != tc.want {
				t.Fatalf("launchLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// ParseNetwork feeds the AM_DEFAULT_NET config value. Spelling variants matter
// because a shell profile, a docs example, and the modal label all naturally
// pick different ones.
func TestParseNetwork(t *testing.T) {
	tests := []struct {
		in   string
		want Network
		ok   bool
	}{
		{"on", NetworkOn, true},
		{"ON", NetworkOn, true},
		{" full ", NetworkOn, true},
		{"danger-full-access", NetworkOn, true},
		{"off", NetworkOff, true},
		{"workspace-write", NetworkOff, true},
		{"confined", NetworkOff, true},
		{"", NetworkOff, false},
		{"yes", NetworkOff, false},
		{"network", NetworkOn, true},
	}
	for _, tc := range tests {
		got, ok := ParseNetwork(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("ParseNetwork(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// ParseApproval accepts codewhale's flag values and the modal's long labels.
func TestParseApproval(t *testing.T) {
	tests := []struct {
		in   string
		want Approval
		ok   bool
	}{
		{"on-request", ApprovalOnRequest, true},
		{"ask", ApprovalOnRequest, true},
		{"auto", ApprovalAuto, true},
		{"never", ApprovalNever, true},
		{"never (unattended)", ApprovalNever, true},
		{"untrusted", ApprovalUntrusted, true},
		{"untrusted (strict)", ApprovalUntrusted, true},
		{"", ApprovalOnRequest, false},
		{"yolo", ApprovalOnRequest, false},
	}
	for _, tc := range tests {
		got, ok := ParseApproval(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("ParseApproval(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
