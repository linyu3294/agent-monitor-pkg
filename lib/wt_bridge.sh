#!/usr/bin/env bash
# wt_bridge.sh — sole point of contact between agent-monitor (parent skill)
# and the worktrees (child skill). Nothing else in this codebase may
# reference the `wt` binary directly.

AM_WT_BIN="${AM_WT_BIN:-$HOME/.claude/skills/worktrees/scripts/wt}"

wt_available() {
  [[ -x "$AM_WT_BIN" ]]
}

wt_list_porcelain() {
  wt_available || return 1
  git worktree list --porcelain 2>/dev/null
}

wt_path_for() {
  local query="${1:-}"
  [[ -n "$query" ]] || return 1
  wt_available || return 1
  "$AM_WT_BIN" path "$query" 2>/dev/null
}

wt_switch() {
  local query="${1:-}"
  [[ -n "$query" ]] || return 1
  wt_available || return 1
  "$AM_WT_BIN" switch "$query"
}

# Create a new worktree and spawn an iTerm tab with cursor-agent running.
#
#   $1 = repo path (absolute, required) — any dir inside the target repo;
#        `wt` will re-exec itself from the primary toplevel.
#   $2 = branch (required)
#   $3 = base ref (default: origin/main)
#   $4 = initial prompt for cursor-agent (optional; empty = no prompt)
#
# Always runs with `--iterm --yolo` because that's the whole point of
# invoking this from the dashboard. Prints `wt`'s stdout/stderr verbatim
# so the caller can surface any errors back to the user.
#
# Return codes:
#   0 = success
#   1 = wt binary unavailable
#   2 = missing required arg
#   3 = repo path doesn't exist or isn't a git repo
#   * = whatever `wt create` returned
wt_create() {
  local repo="${1:-}"
  local branch="${2:-}"
  local base="${3:-origin/main}"
  local prompt="${4:-}"
  [[ -n "$repo" && -n "$branch" ]] || return 2
  wt_available || return 1
  [[ -d "$repo" ]] || return 3

  local args=(create "$branch" --base "$base" --iterm --yolo)
  if [[ -n "$prompt" ]]; then
    args+=(--prompt "$prompt")
  fi
  # Run `wt` from inside the target repo; wt re-execs from the primary
  # toplevel so any dir inside the repo works (including a worktree).
  (cd "$repo" && "$AM_WT_BIN" "${args[@]}")
}

# Open the nvim + :DiffviewOpen iTerm tab for an EXISTING worktree
# (not a new one). Used by the dashboard's `i` hotkey when the user
# wants a VS Code-style CHANGES sidebar on an already-running session.
#
#   $1 = repo path (absolute, required) — any dir inside the repo;
#        `wt` re-execs from the primary toplevel internally.
#   $2 = worktree slug or branch name (required).
#
# Return codes:
#   0 = tab launched (or WT_NO_OPEN=1 / WT_NO_NVIM=1 silently honored)
#   1 = wt binary unavailable
#   2 = missing required arg
#   3 = repo path doesn't exist
#   * = whatever `wt ide` returned (missing nvim, bad slug, etc.)
#
# Prints `wt`'s stdout/stderr verbatim so the caller can surface errors.
wt_open_ide() {
  local repo="${1:-}"
  local slug="${2:-}"
  [[ -n "$repo" && -n "$slug" ]] || return 2
  wt_available || return 1
  [[ -d "$repo" ]] || return 3
  (cd "$repo" && "$AM_WT_BIN" ide "$slug")
}

# Resume a cursor-agent session in a fresh iTerm tab rooted at the
# session's workspace cwd (worktree path for worktree-rooted sessions,
# primary repo root otherwise). Used by the dashboard's `a` hotkey.
#
#   $1 = session id (required, must be a UUIDv4)
#   $2 = cwd on disk (required, absolute path) — the session's workspace
#        root. Pass whatever `sessions_repo_path_at` returns.
#
# Return codes:
#   0 = iTerm tab launched (or WT_NO_OPEN=1 silently honored)
#   1 = wt binary unavailable
#   2 = missing required arg
#   3 = cwd doesn't exist
#   * = whatever `wt resume` returned (bad id format, missing cursor-agent, …)
#
# Prints `wt`'s stdout/stderr verbatim so the caller can surface errors.
wt_resume_in_iterm() {
  local id="${1:-}" cwd="${2:-}"
  [[ -n "$id" && -n "$cwd" ]] || return 2
  wt_available || return 1
  [[ -d "$cwd" ]] || return 3
  # `wt` internally re-execs from the primary toplevel, so we need to
  # invoke it from *any* dir that's inside a git repo. The session's
  # workspace cwd qualifies whether it's a worktree or the primary.
  (cd "$cwd" && "$AM_WT_BIN" resume "$id" --cwd "$cwd")
}

wt_slug_for_path() {
  local path="${1:-}"
  [[ -n "$path" ]] || return 1
  if [[ "$path" == */.worktrees/* ]]; then
    printf '%s\n' "${path##*/.worktrees/}"
    return 0
  fi
  return 1
}

# Remove a worktree (and its branch, unless --keep-branch).
#
#   $1 = repo path (absolute, required) — used so `wt` runs inside the
#        correct git repo's primary toplevel.
#   $2 = slug or branch name (required).
#   $3 = "force" (optional) — if set to the literal string "force", pass
#        `--force` to `wt remove` so git won't refuse on dirty/untracked
#        state and the branch is deleted via `git branch -D`.
#
# Return codes:
#   0 = success
#   1 = wt binary unavailable
#   2 = missing required arg
#   3 = repo path doesn't exist
#   * = whatever `wt remove` returned
#
# Prints `wt`'s stdout/stderr verbatim so the caller can surface errors.
wt_remove() {
  local repo="${1:-}"
  local slug="${2:-}"
  local force="${3:-}"
  [[ -n "$repo" && -n "$slug" ]] || return 2
  wt_available || return 1
  [[ -d "$repo" ]] || return 3

  local args=(remove "$slug")
  [[ "$force" == "force" ]] && args+=(--force)
  (cd "$repo" && "$AM_WT_BIN" "${args[@]}")
}
