package wtbridge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file holds the git-native implementations of every dashboard
// action. They exist because `wt` is an optional skill: the dashboard
// must still resume, rebase, remove, and open worktrees on a machine
// where the worktrees skill was never installed. `wt` remains the
// preferred path when present — see the methods in wtbridge.go — but it
// is no longer a prerequisite.

// worktreePathFor is the on-disk location of a worktree slug. Create
// uses this layout, the scanner scans it, and `wt` uses it too, so it is
// the one convention worth relying on.
func worktreePathFor(repo, slug string) string {
	return filepath.Join(repo, ".worktrees", slug)
}

// isDir reports whether p is an existing directory.
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// gitOut runs git in dir and returns trimmed stdout.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// refExists reports whether ref resolves to a commit in repo.
func refExists(repo, ref string) bool {
	if ref == "" {
		return false
	}
	_, err := gitOut(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// DefaultBase returns the ref a new worktree should branch from, or ""
// when the repo has no commits yet.
//
// The `n` modal used to hardcode `origin/main`, which fails outright on
// any repo whose default is `master`, whose remote is named something
// else, or which has no remote at all. The dashboard should read the
// repo instead of assuming.
func DefaultBase(repo string) string {
	if repo == "" {
		return ""
	}
	// origin/HEAD is the remote's own answer and follows a renamed
	// default branch; resolve it to the concrete ref for a readable
	// value in the modal.
	if ref, err := gitOut(repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		if short := strings.TrimPrefix(ref, "refs/remotes/"); refExists(repo, short) {
			return short
		}
	}
	for _, ref := range []string{"origin/main", "origin/master", "main", "master", "HEAD"} {
		if refExists(repo, ref) {
			return ref
		}
	}
	return ""
}

// resolveBase picks the base to branch from, honoring the requested ref
// when it exists and otherwise reporting what was available.
func resolveBase(repo, requested string) (string, error) {
	if refExists(repo, requested) {
		return requested, nil
	}
	if ref := DefaultBase(repo); ref != "" {
		return ref, nil
	}
	return "", fmt.Errorf("no base branch found in %s (no commits yet?)", repo)
}

// branchAt returns the branch checked out at dir, or "" when detached.
func branchAt(dir string) string {
	name, err := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || name == "HEAD" {
		return ""
	}
	return name
}

// primaryOf returns the primary checkout for a worktree path.
func primaryOf(dir string) string {
	common, err := gitOut(dir, "rev-parse", "--git-common-dir")
	if err != nil || common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		// Relative to dir, e.g. ".git" for a primary checkout.
		if abs, err := filepath.Abs(filepath.Join(dir, common)); err == nil {
			common = abs
		}
	}
	return filepath.Dir(common)
}

// gitRemoveWorktree deletes a worktree directory and, when it is not the
// default branch, the branch it had checked out.
//
// Order matters: the branch cannot be deleted while the worktree still
// has it checked out, so the worktree goes first. A failed removal
// (dirty tree without force) returns before touching the branch, so a
// refused delete never leaves a half-removed worktree with its branch
// gone.
func gitRemoveWorktree(repo, slug string, force bool) error {
	path := worktreePathFor(repo, slug)
	if !isDir(path) {
		return fmt.Errorf("worktree %q not found at %s", slug, path)
	}
	branch := branchAt(path)

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if out, err := runGit(repo, args...); err != nil {
		msg := firstLine(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	if branch == "" {
		return nil
	}
	for _, protected := range []string{"main", "master", "trunk", "develop"} {
		if branch == protected {
			return nil
		}
	}
	if out, err := runGit(repo, "branch", "-D", branch); err != nil {
		msg := firstLine(string(out))
		if msg == "" {
			msg = err.Error()
		}
		// The worktree is already gone; report the branch failure but
		// don't pretend the removal didn't happen.
		return fmt.Errorf("worktree removed, but deleting branch %q failed: %s", branch, msg)
	}
	return nil
}

// gitSync rebases a worktree onto its upstream, or onto the repo's
// default base when the branch tracks nothing.
//
// A fetch failure is not fatal: sync is still useful offline, rebasing
// onto whatever refs are already local.
func gitSync(dir string) error {
	if !isDir(dir) {
		return fmt.Errorf("not a directory: %s", dir)
	}
	_, _ = gitOut(dir, "fetch", "--prune", "--quiet")

	base := ""
	if up, err := gitOut(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil && up != "" {
		base = up
	}
	if base == "" {
		primary := primaryOf(dir)
		if primary == "" {
			primary = dir
		}
		base = DefaultBase(primary)
	}
	if base == "" {
		return fmt.Errorf("no upstream or base branch to sync %s onto", dir)
	}
	if out, err := runGit(dir, "rebase", base); err != nil {
		msg := firstLine(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("rebase onto %s failed: %s", base, msg)
	}
	return nil
}

// gitOpenWorktree reveals a worktree in Cursor.
func gitOpenWorktree(repo, slug string) error {
	path := worktreePathFor(repo, slug)
	if !isDir(path) {
		return fmt.Errorf("worktree %q not found at %s", slug, path)
	}
	if err := exec.Command("open", "-a", "Cursor", path).Run(); err != nil {
		return fmt.Errorf("could not open %s in Cursor: %w", path, err)
	}
	return nil
}

// gitIDE opens nvim with Diffview in a new iTerm tab at the worktree.
// Kept behind the same tab path Create uses so the panes behave the same.
func gitIDE(repo, slug string) error {
	path := worktreePathFor(repo, slug)
	if !isDir(path) {
		return fmt.Errorf("worktree %q not found at %s", slug, path)
	}
	return OpenITerm(path, "nvim -c DiffviewOpen")
}
