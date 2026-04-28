#!/usr/bin/env bash
# uninstall.sh — remove agent-monitor from the machine.
#
# What this does:
#   1. Kills the agent-monitor tmux session if running.
#   2. Removes the ~/.local/bin/am symlink.
#   3. Optionally rm -rf $AM_HOME (with y/N confirm).
#
# Never touches monitored cursor-agent processes, transcripts, or
# worktrees — those belong to Cursor / wt and outlive this tool.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AM_HOME="${AM_HOME:-$HERE}"
AM_TMUX_SESSION="${AM_TMUX_SESSION:-agent-monitor}"

step() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m  %s\n' "$*" >&2; }

if command -v tmux >/dev/null 2>&1; then
  if tmux has-session -t "$AM_TMUX_SESSION" 2>/dev/null; then
    step "killing tmux session '$AM_TMUX_SESSION' (monitored agents unaffected)"
    tmux kill-session -t "$AM_TMUX_SESSION" || true
  fi
fi

local_bin="$HOME/.local/bin/am"
if [[ -L "$local_bin" ]]; then
  step "removing $local_bin symlink"
  rm -f "$local_bin"
elif [[ -e "$local_bin" ]]; then
  warn "$local_bin exists but is not a symlink — leaving it alone"
fi

read -r -p "rm -rf $AM_HOME ? (this deletes the source tree and binaries) [y/N] " resp
case "$resp" in
  y|Y|yes|YES)
    step "removing $AM_HOME"
    rm -rf "$AM_HOME"
    ;;
  *)
    step "leaving $AM_HOME in place"
    ;;
esac

echo
echo "agent-monitor uninstalled."
