// Package wtbridge is the sole point of contact between the dashboard
// and the worktrees skill's `wt` CLI.
//
// We shell out to `wt` when it is installed, because it is the canonical
// source of truth for worktree state on this machine. Create falls back
// to `git worktree` under `<repo>/.worktrees/` when `wt` is absent, which
// is the same layout the dashboard already scans.
package wtbridge

import (
	"bytes"
	"errors"
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
func (b *Bridge) Resume(id, cwd string) error {
	if !b.Available() {
		return errors.New("wt binary unavailable (install the worktrees skill or set AM_WT_BIN)")
	}
	return runIn(cwd, b.bin, "resume", id, "--cwd", cwd)
}

// Create spawns a new worktree at <branch> based on <base> and opens an
// iTerm tab running cursor-agent. When the wt CLI is installed this shells
// out to `wt create --iterm --yolo`. Otherwise it creates
// `<repo>/.worktrees/<slug>` with git directly — the layout the dashboard
// already scans — so `n` still works on machines without the worktrees skill.
// <prompt> may be empty.
func (b *Bridge) Create(repo, branch, base, prompt string) error {
	if b.Available() {
		args := []string{"create", branch, "--base", base, "--iterm", "--yolo"}
		if prompt != "" {
			args = append(args, "--prompt", prompt)
		}
		return runIn(repo, b.bin, args...)
	}
	return createWithGit(repo, branch, base, prompt)
}

// GitPrimary returns the main checkout for the git repo containing dir.
// Worktree directories resolve to the primary repo, not the linked tree.
// Returns "" when dir is not inside a git repo.
func GitPrimary(dir string) string {
	if strings.TrimSpace(dir) == "" {
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

func createWithGit(repo, branch, base, prompt string) error {
	top := GitPrimary(repo)
	if top == "" {
		return fmt.Errorf("not a git repo: %s", repo)
	}
	slug := slugFromBranch(branch)
	dest := filepath.Join(top, ".worktrees", slug)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := runGit(top, "worktree", "add", "-b", branch, dest, base)
	if err != nil && bytes.Contains(out, []byte("branch named")) && bytes.Contains(out, []byte("already exists")) {
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
	cmd := "cursor-agent --yolo"
	if strings.TrimSpace(prompt) != "" {
		cmd += " -- " + shellQuote(prompt)
	}
	if err := OpenITerm(dest, cmd); err != nil {
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
	if cwd == "" {
		cwd = "$HOME"
	}
	full := "cd " + shellQuote(cwd) + " && " + cmd
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

// Remove deletes the named worktree (and its branch) via `wt remove`.
// If force is true, passes `--force` so dirty/untracked state and
// branch-not-merged checks are bypassed.
func (b *Bridge) Remove(repo, slug string, force bool) error {
	if !b.Available() {
		return errors.New("wt binary unavailable")
	}
	args := []string{"remove", slug}
	if force {
		args = append(args, "--force")
	}
	return runIn(repo, b.bin, args...)
}

// Sync runs `wt sync` (rebase the current worktree onto its base).
func (b *Bridge) Sync(repo string) error {
	if !b.Available() {
		return errors.New("wt binary unavailable")
	}
	return runIn(repo, b.bin, "sync")
}

// IDE opens an nvim+Diffview iTerm tab for an existing worktree.
func (b *Bridge) IDE(repo, slug string) error {
	if !b.Available() {
		return errors.New("wt binary unavailable")
	}
	return runIn(repo, b.bin, "ide", slug)
}

// Open opens the worktree in Cursor (the IDE).
func (b *Bridge) Open(repo, slug string) error {
	if !b.Available() {
		return errors.New("wt binary unavailable")
	}
	return runIn(repo, b.bin, "open", slug)
}

// PathFor resolves a worktree slug to its absolute on-disk path via `wt path`.
func (b *Bridge) PathFor(repo, slug string) (string, error) {
	if !b.Available() {
		return "", errors.New("wt binary unavailable")
	}
	cmd := exec.Command(b.bin, "path", slug)
	if repo != "" {
		cmd.Dir = repo
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wt path failed: %w", err)
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
