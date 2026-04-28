// Package wtbridge is the sole point of contact between the dashboard
// and the worktrees skill's `wt` CLI.
//
// We shell out rather than re-implement worktree management because (a)
// `wt` is the canonical source of truth for worktree state on this
// machine, and (b) shelling out keeps `agent-monitor` agnostic to `wt`'s
// internal layout — schema changes there don't break us.
package wtbridge

import (
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
// iTerm tab + IDE tab via `wt create --iterm --yolo`. <prompt> may be
// empty.
func (b *Bridge) Create(repo, branch, base, prompt string) error {
	if !b.Available() {
		return errors.New("wt binary unavailable")
	}
	args := []string{"create", branch, "--base", base, "--iterm", "--yolo"}
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	return runIn(repo, b.bin, args...)
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
