// Package wtbridge is the sole point of contact between the dashboard
// and the machine's worktree tooling.
//
// `wt` (the worktrees skill) is preferred when it is installed, because
// it is the canonical source of truth for worktree state there. It is
// not required: every action has a git-native implementation in
// gitops.go, using the `<repo>/.worktrees/<slug>` layout that Create,
// the scanner, and `wt` all agree on. A machine without the skill can
// still create, resume, rebase, open, and remove worktrees.
package wtbridge

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Bridge wraps a discovered `wt` binary path. Call New() to create one;
// the returned Bridge will be Available()=false if wt isn't installed,
// and methods that need wt will fail with a clean error.
type Bridge struct {
	bin string
}

// New resolves the wt binary path. Honors $AM_WT_BIN, otherwise falls
// back to the canonical install path under ~/.claude/skills/worktrees/.
func New() *Bridge {
	bin := os.Getenv("AM_WT_BIN")
	if bin == "" {
		home, _ := os.UserHomeDir()
		bin = filepath.Join(home, ".claude", "skills", "worktrees", "scripts", "wt")
	}
	if _, err := os.Stat(bin); err != nil {
		return &Bridge{bin: ""}
	}
	return &Bridge{bin: bin}
}

// Available is true when the wt binary is on disk and executable.
func (b *Bridge) Available() bool { return b.bin != "" }

// Resume opens a fresh iTerm tab cded into <cwd>, running
// `cursor-agent --resume <id>`. <cwd> should be the session's workspace
// root (worktree path or primary repo).
//
// Without `wt` this is the same thing wt would have done: one tab, one
// command. Resume never needed the skill, only a way to open a tab.
func (b *Bridge) Resume(id, cwd string) error {
	if b.Available() {
		return runIn(cwd, b.bin, "resume", id, "--cwd", cwd)
	}
	return OpenITerm(cwd, "cursor-agent --resume "+shellQuote(id))
}

// Agent identifies which CLI the `n` modal launches inside a fresh
// worktree. Cursor preserves the historical behavior; Codewhale opens an
// interactive Codewhale session in the same tab.
type Agent int

const (
	// AgentCursor launches `cursor-agent`.
	AgentCursor Agent = iota
	// AgentCodewhale launches `codewhale`, confined to the new worktree.
	AgentCodewhale
)

// agents is the launcher order shown in the `n` modal.
var agents = []Agent{AgentCursor, AgentCodewhale}

// String is the label rendered in the modal and in status toasts.
func (a Agent) String() string {
	if a == AgentCodewhale {
		return "codewhale"
	}
	return "cursor-agent"
}

// Bin is the executable this agent launches.
func (a Agent) Bin() string {
	if a == AgentCodewhale {
		return "codewhale"
	}
	return "cursor-agent"
}

// Available reports whether the launcher binary is on PATH. The modal
// checks this before creating anything, so a missing CLI fails inline
// instead of leaving an orphan worktree with a dead tab.
func (a Agent) Available() bool {
	_, err := exec.LookPath(a.Bin())
	return err == nil
}

// Next returns the agent delta places away in agents, wrapping around.
// delta may be negative (the modal's left arrow).
func (a Agent) Next(delta int) Agent {
	n := len(agents)
	idx := 0
	for i, cand := range agents {
		if cand == a {
			idx = i
			break
		}
	}
	idx = ((idx+delta)%n + n) % n
	return agents[idx]
}

// Approval is the tool-approval posture a newly launched agent runs
// with. It is chosen per launch by the person using the dashboard, never
// imposed by it: an agent that can silently run commands without asking
// is a decision the human makes, not a default a viewer sets for them.
//
// The zero value is ApprovalOnRequest, so a zero-valued config, a
// forgotten field, or a caller that predates this type all land on the
// posture that keeps the human in the loop.
type Approval int

const (
	// ApprovalOnRequest asks the user before acting. Default.
	ApprovalOnRequest Approval = iota
	// ApprovalAuto hands approval decisions to the agent's own policy —
	// the middle ground between being asked about everything and never
	// being asked at all.
	ApprovalAuto
	// ApprovalNever runs unattended, for a worktree the user opens and
	// walks away from. Explicit opt-in only.
	ApprovalNever
	// ApprovalUntrusted refuses unless the request is already trusted.
	ApprovalUntrusted
)

// approvals is the cycle order shown in the `n` modal: least to most
// permissive, then the strictest, so ←always lands somewhere calmer.
var approvals = []Approval{ApprovalOnRequest, ApprovalAuto, ApprovalNever, ApprovalUntrusted}

// String is the label rendered in the modal.
func (a Approval) String() string {
	switch a {
	case ApprovalAuto:
		return "auto"
	case ApprovalNever:
		return "never (unattended)"
	case ApprovalUntrusted:
		return "untrusted (strict)"
	}
	return "on-request"
}

// Flag is the CLI value for codewhale's --approval-policy.
//
// The accepted enum is exactly: on-request, untrusted, never, auto,
// suggest. Verified against the installed binary via
// `codewhale --approval-policy <v> doctor`, which reports the same
// validation error the launch path does.
//
// This must not be confused with `codewhale sandbox check --ask`, which
// takes a *different* set (unless-trusted, on-failure, on-request,
// never). Mapping auto to `on-failure` because the sandbox checker
// accepts it produced a launch that failed immediately with "Invalid
// approval_policy 'on-failure'". Note also that `codewhale
// --approval-policy <anything> sessions list` exits 0 — the subversion
// path does not validate the global option, so it is not a usable check.
func (a Approval) Flag() string {
	switch a {
	case ApprovalAuto:
		return "auto"
	case ApprovalNever:
		return "never"
	case ApprovalUntrusted:
		return "untrusted"
	}
	return "on-request"
}

// Next returns the approval delta places away, wrapping around.
func (a Approval) Next(delta int) Approval {
	n := len(approvals)
	idx := 0
	for i, cand := range approvals {
		if cand == a {
			idx = i
			break
		}
	}
	idx = ((idx+delta)%n + n) % n
	return approvals[idx]
}

// Network is the network posture a newly launched agent runs with.
//
// It is separate from Approval because the two are genuinely different
// questions — "may you run this?" and "can you reach the internet?" — and
// conflating them would mean granting disk-wide access just to answer a
// question, or reaching the network just to answer a prompt.
//
// The zero value is NetworkOff, so a zero-valued config, a forgotten field,
// or a caller that predates this type keeps the confined default.
type Network int

const (
	// NetworkOff confines the agent to its worktree with no egress. Default.
	NetworkOff Network = iota
	// NetworkOn gives the agent egress. It is explicit opt-in only: an agent
	// that can reach the internet can also reach any credential the shell
	// already holds, so the human decides per launch.
	NetworkOn
)

// networks is the cycle order shown in the `n` modal: confined first, so a
// stray ←/→ lands back on the safer value.
var networks = []Network{NetworkOff, NetworkOn}

// String is the label rendered in the modal. It states what the setting
// actually does rather than naming the network alone, because for codewhale
// there is no network-only mode: egress comes with danger-full-access, which
// lifts the filesystem confinement too. Calling this "network: on" would be a
// promise the CLI cannot keep.
func (n Network) String() string {
	if n == NetworkOn {
		return "on (full access)"
	}
	return "off (workspace-write)"
}

// Next returns the network delta places away, wrapping around.
func (n Network) Next(delta int) Network {
	idx := 0
	for i, cand := range networks {
		if cand == n {
			idx = i
			break
		}
	}
	idx = ((idx+delta)%len(networks) + len(networks)) % len(networks)
	return networks[idx]
}

// ParseNetwork reads a configured network posture. The accepted spellings are
// the modal labels without the parenthetical, plus their obvious synonyms, so a
// shell profile can say AM_DEFAULT_NET=on and a config file can say
// "workspace-write" without either being a special case.
//
// The second return distinguishes "unrecognised" from "explicitly off". The
// caller needs that: silently falling back to off would hide a typo in exactly
// the setting where a typo is most costly to diagnose.
func ParseNetwork(s string) (Network, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "full", "full-access", "danger-full-access", "network":
		return NetworkOn, true
	case "off", "confined", "workspace-write", "none":
		return NetworkOff, true
	}
	return NetworkOff, false
}

// ParseApproval reads a configured approval posture. It accepts the same
// values as codewhale's --approval-policy plus the modal's long labels.
func ParseApproval(s string) (Approval, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on-request", "ask":
		return ApprovalOnRequest, true
	case "auto":
		return ApprovalAuto, true
	case "never", "never (unattended)", "unattended":
		return ApprovalNever, true
	case "untrusted", "untrusted (strict)", "strict":
		return ApprovalUntrusted, true
	}
	return ApprovalOnRequest, false
}

// LaunchCommand is LaunchCommandWith at the confined network posture, which is
// the shape every pre-existing caller expects.
func (a Agent) LaunchCommand(prompt string, approval Approval) string {
	return a.LaunchCommandWith(prompt, approval, NetworkOff)
}

// LaunchCommandWith is the shell line typed into the new iTerm tab.
//
// The sandbox stays at workspace-write so the agent can edit freely inside the
// new worktree without reaching the rest of the machine, unless the user asked
// for egress. The approval posture is whatever the user picked:
//
//	on-request  codewhale --approval-policy on-request | cursor-agent (default)
//	auto        codewhale --approval-policy auto        | cursor-agent --auto-review
//	never       codewhale --approval-policy never       | cursor-agent --yolo
//	untrusted   codewhale --approval-policy untrusted   | cursor-agent (default)
//
// cursor-agent is "Smart Auto" under --auto-review: a classifier runs safe tool
// calls and prompts for the rest. It has no stricter-than-default flag, so
// untrusted maps to its default behaviour rather than inventing one.
//
// NetworkOn maps to the widest mode each CLI has, because neither offers egress
// without it: codewhale's --sandbox-mode values are read-only,
// workspace-write, danger-full-access and external-sandbox, and danger-full-
// access is documented as disabling the sandbox entirely. cursor-agent takes
// --sandbox enabled|disabled. When the posture is off, cursor-agent gets no
// flag at all — its effective default is whatever its config says, and adding
// `--sandbox enabled` would change behaviour for users who never asked for it.
func (a Agent) LaunchCommandWith(prompt string, approval Approval, network Network) string {
	cmd := "cursor-agent"
	switch a {
	case AgentCodewhale:
		sandbox := "workspace-write"
		if network == NetworkOn {
			sandbox = "danger-full-access"
		}
		cmd = "codewhale --approval-policy " + approval.Flag() +
			" --sandbox-mode " + sandbox + " --skip-onboarding --fresh"
	default:
		switch approval {
		case ApprovalAuto:
			cmd += " --auto-review"
		case ApprovalNever:
			cmd += " --yolo"
		}
		if network == NetworkOn {
			cmd += " --sandbox disabled"
		}
	}
	if strings.TrimSpace(prompt) != "" {
		cmd += " -- " + shellQuote(prompt)
	}
	return cmd
}

// LaunchOptions is everything the `n` modal decides about a new agent. It is a
// struct rather than a parameter list because the list has grown past the point
// where positional arguments stay readable — two postures plus two optional
// strings is where a swapped argument stops being obvious at the call site.
//
// The zero value is the confined, asks-first default that Create has always
// used, so an unset field cannot widen what an agent may do.
type LaunchOptions struct {
	Agent    Agent
	Approval Approval
	Network  Network
	// Before is an optional shell command run in the worktree before the agent.
	Before string
	// Prompt is passed to the agent as its opening instruction.
	Prompt string
}

// wtExpressible reports whether `wt create --iterm` can express this launch.
// wt knows only --yolo and --prompt, so it silently drops anything else: the
// auto posture, a setup command, or a network request would each come out the
// other side as a launch the user did not choose. Those go through git instead.
func (o LaunchOptions) wtExpressible() bool {
	return o.Agent == AgentCursor &&
		o.Approval != ApprovalAuto &&
		o.Network == NetworkOff &&
		strings.TrimSpace(o.Before) == ""
}

// Create spawns a new worktree at <branch> based on <base> and opens an iTerm
// tab running <agent> with the chosen <approval> posture. It is
// CreateWithOptions with the fields that predate it.
func (b *Bridge) Create(repo, branch, base, prompt string, agent Agent, approval Approval) error {
	return b.CreateWithOptions(repo, branch, base, LaunchOptions{
		Agent:    agent,
		Approval: approval,
		Prompt:   prompt,
	})
}

// CreateWithOptions spawns the worktree and opens the tab. Everything except a
// launch wt can express is created as `<repo>/.worktrees/<slug>` with git
// directly — the layout the dashboard already scans — and the tab is opened
// here, so `n` works without the worktrees skill and non-cursor launchers are
// not forced through wt.
func (b *Bridge) CreateWithOptions(repo, branch, base string, opts LaunchOptions) error {
	if b.Available() && opts.wtExpressible() {
		args := []string{"create", branch, "--base", base, "--iterm"}
		if opts.Approval == ApprovalNever {
			args = append(args, "--yolo")
		}
		if opts.Prompt != "" {
			args = append(args, "--prompt", opts.Prompt)
		}
		return runIn(repo, b.bin, args...)
	}
	return createWithGit(repo, branch, base, opts)
}

// GitPrimary returns the main checkout for the git repo containing dir.
// Worktree directories resolve to the primary repo, not the linked tree.
// Returns "" when dir is not inside a git repo.
func GitPrimary(dir string) string {
	dir = expandUserPath(dir)
	if dir == "" {
		return ""
	}
	topOut, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(topOut))
	commonOut, err := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return top
	}
	common := strings.TrimSpace(string(commonOut))
	if !filepath.IsAbs(common) {
		return top
	}
	return filepath.Dir(common)
}

// expandUserPath turns a typed repo path into an absolute path. The modal
// accepts "~/Documents/credit-platform/", but git does not expand ~.
func expandUserPath(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		if dir == "~" {
			dir = home
		} else {
			dir = filepath.Join(home, dir[2:])
		}
	}
	return filepath.Clean(dir)
}

func createWithGit(repo, branch, base string, opts LaunchOptions) error {
	top := GitPrimary(repo)
	if top == "" {
		return fmt.Errorf("not a git repo: %s", repo)
	}
	resolved, err := resolveBase(top, base)
	if err != nil {
		return err
	}
	slug := slugFromBranch(branch)
	dest := worktreePathFor(top, slug)
	if isDir(dest) {
		return fmt.Errorf("worktree %q already exists at %s — remove it first, or pick another branch", slug, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := runGit(top, "worktree", "add", "-b", branch, dest, resolved)
	if err != nil && bytes.Contains(out, []byte("branch named")) && bytes.Contains(out, []byte("already exists")) {
		// The branch exists but isn't checked out anywhere: reuse it.
		out, err = runGit(top, "worktree", "add", dest, branch)
	}
	if err != nil {
		msg := firstLine(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	if os.Getenv("WT_NO_OPEN") == "1" {
		return nil
	}
	if err := OpenITermWithSetup(dest, opts.Before, opts.Agent.LaunchCommandWith(opts.Prompt, opts.Approval, opts.Network)); err != nil {
		return fmt.Errorf("worktree created at %s, but opening iTerm failed: %w", dest, err)
	}
	return nil
}

func slugFromBranch(branch string) string {
	s := strings.TrimSpace(branch)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, " ", "-")
	if s == "" || s == "." || s == ".." {
		return "worktree"
	}
	return s
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// OpenITerm spawns a new iTerm2 tab cd'ing into cwd and running cmd.
// Best-effort — returns an AppleScript error verbatim if iTerm refuses.
func OpenITerm(cwd, cmd string) error {
	return openITermLine(cwd, "", cmd)
}

// OpenITermWithSetup is OpenITerm with a command run after the cd and before
// cmd. The two are joined with && rather than ; on purpose: a failed setup means
// the agent never starts, so a half-prepared environment cannot be mistaken for
// a working one. The tab stops at the shell prompt with the error on screen.
func OpenITermWithSetup(cwd, before, cmd string) error {
	return openITermLine(cwd, before, cmd)
}

func openITermLine(cwd, before, cmd string) error {
	full := launchLine(cwd, before, cmd)
	asLine := strings.ReplaceAll(full, `\`, `\\`)
	asLine = strings.ReplaceAll(asLine, `"`, `\"`)
	script := fmt.Sprintf(`tell application "iTerm"
  activate
  if (count of windows) = 0 then
    set newWindow to (create window with default profile)
    tell current session of newWindow to write text "%s"
  else
    tell current window
      set newTab to (create tab with default profile)
    end tell
    tell current session of newTab to write text "%s"
  end if
end tell`, asLine, asLine)
	return exec.Command("osascript", "-e", script).Run()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// launchLine composes what gets typed into a new tab: cd into the worktree, the
// optional setup command, then the agent. && throughout, so a failed cd or a
// failed setup leaves the agent unlaunched with the error still on screen.
func launchLine(cwd, before, cmd string) string {
	if cwd == "" {
		cwd = "$HOME"
	}
	full := "cd " + shellQuote(cwd)
	if setup := strings.TrimSpace(before); setup != "" {
		full += " && " + setup
	}
	return full + " && " + cmd
}

// Remove deletes the named worktree and its branch. If force is true,
// dirty/untracked state and branch-not-merged checks are bypassed.
func (b *Bridge) Remove(repo, slug string, force bool) error {
	if !b.Available() {
		return gitRemoveWorktree(repo, slug, force)
	}
	args := []string{"remove", slug}
	if force {
		args = append(args, "--force")
	}
	return runIn(repo, b.bin, args...)
}

// Sync rebases the worktree at <repo> onto its base.
func (b *Bridge) Sync(repo string) error {
	if !b.Available() {
		return gitSync(repo)
	}
	return runIn(repo, b.bin, "sync")
}

// IDE opens an nvim+Diffview iTerm tab for an existing worktree.
func (b *Bridge) IDE(repo, slug string) error {
	if !b.Available() {
		return gitIDE(repo, slug)
	}
	return runIn(repo, b.bin, "ide", slug)
}

// Open opens the worktree in Cursor (the IDE).
func (b *Bridge) Open(repo, slug string) error {
	if !b.Available() {
		return gitOpenWorktree(repo, slug)
	}
	return runIn(repo, b.bin, "open", slug)
}

// PathFor resolves a worktree slug to its absolute on-disk path.
//
// The filesystem is checked first: the directory either exists in the
// agreed layout or it doesn't, and that answer is cheaper and more
// reliable than asking a tool that may not be installed.
func (b *Bridge) PathFor(repo, slug string) (string, error) {
	path := worktreePathFor(repo, slug)
	if isDir(path) {
		return path, nil
	}
	if !b.Available() {
		return "", fmt.Errorf("worktree %q not found at %s", slug, path)
	}
	cmd := exec.Command(b.bin, "path", slug)
	if repo != "" {
		cmd.Dir = repo
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("worktree %q not found at %s", slug, path)
	}
	return strings.TrimSpace(string(out)), nil
}

// runIn executes a command in <dir> and returns its first stderr line on
// failure as the error text (since `wt` prints actionable messages).
func runIn(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	first := firstLine(string(out))
	if first == "" {
		return err
	}
	return fmt.Errorf("%s (exit: %v)", first, err)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
