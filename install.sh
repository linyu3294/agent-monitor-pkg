#!/usr/bin/env bash
# install.sh — set up agent-monitor on a fresh machine.
#
# What this does:
#   1. Verifies the machine looks like macOS with the binaries we need.
#   2. Picks the right arch (darwin-arm64 vs darwin-amd64).
#   3. Builds the Go binary if `go` is on PATH; otherwise downloads the
#      matching prebuilt binary from the latest GitHub release.
#   4. Symlinks $AM_HOME/am into ~/.local/bin/am so `am` is on the
#      user's PATH.
#
# Idempotent: safe to re-run. Re-running is the same as `am update`
# minus the `git pull` (the user controls when the source updates).
#
# The script never edits the user's shell rc files. If ~/.local/bin
# isn't on PATH it just prints the export line for the user to add.

set -euo pipefail

OWNER="${AM_GITHUB_OWNER:-agent-monitor}"   # override at install time
REPO="${AM_GITHUB_REPO:-agent-monitor}"
RELEASE="${AM_RELEASE_TAG:-latest}"

# --- self-locating ---
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AM_HOME="${AM_HOME:-$HERE}"

DOWNLOAD_ONLY=0
case "${1:-}" in
  --download-only) DOWNLOAD_ONLY=1 ;;
  -h|--help)
    cat <<EOF
install.sh — set up agent-monitor

Usage:
  ./install.sh                  build (or download) + install symlink
  ./install.sh --download-only  download prebuilt binary only, skip symlink

Env overrides:
  AM_HOME              install root (default: dir containing install.sh)
  AM_GITHUB_OWNER      GitHub owner for prebuilt binary releases
  AM_GITHUB_REPO       GitHub repo
  AM_RELEASE_TAG       release tag (default: latest)

Once installed, run:
  am help
EOF
    exit 0
    ;;
esac

step() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m  %s\n' "$*" >&2; }
fail() { printf '\033[1;31mxx\033[0m  %s\n' "$*" >&2; exit 1; }

# Refuse to run anywhere we haven't tested.
[[ "$(uname -s)" == "Darwin" ]] || fail "agent-monitor currently supports macOS only"

# Required tools: tmux + ps + lsof. iTerm2/nvim/wt are optional and
# warned about, not blocked.
for cmd in tmux ps lsof; do
  command -v "$cmd" >/dev/null 2>&1 || fail "missing required tool: $cmd (try: brew install $cmd)"
done
[[ -d /Applications/iTerm.app ]] || warn "iTerm2 not found at /Applications/iTerm.app — 'a' / 'n' / 'i' hotkeys won't open tabs"
command -v cursor-agent >/dev/null 2>&1 || warn "cursor-agent not on PATH — 'a' resume tabs will fail to launch"
[[ -x "$HOME/.claude/skills/worktrees/scripts/wt" ]] || warn "worktrees skill not installed — 'n'/'r'/'s'/'i'/'o' hotkeys won't work"

# Arch detection.
arch="$(uname -m)"
case "$arch" in
  arm64|aarch64) arch="arm64" ;;
  x86_64) arch="amd64" ;;
  *) fail "unsupported arch: $arch" ;;
esac
binary_name="agent-monitor-darwin-${arch}"
binary_path="$AM_HOME/bin/$binary_name"

mkdir -p "$AM_HOME/bin"

if command -v go >/dev/null 2>&1; then
  step "go found ($(go version | awk '{print $3}')) — building from source"
  pushd "$AM_HOME/go" >/dev/null
  version="dev"
  if [[ -d "$AM_HOME/.git" ]] && command -v git >/dev/null 2>&1; then
    version="$(git -C "$AM_HOME" describe --tags --always --dirty 2>/dev/null || echo dev)"
  fi
  go build \
    -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "$binary_path" \
    ./cmd/agent-monitor
  popd >/dev/null
  step "built $binary_path"
else
  step "go not found — downloading prebuilt $binary_name from GitHub release '$RELEASE'"
  url="https://github.com/${OWNER}/${REPO}/releases/${RELEASE}/download/${binary_name}"
  if ! curl --fail --location --output "$binary_path" "$url" 2>/dev/null; then
    fail "download failed: $url
hint: install go (brew install go) and re-run install.sh, or check the release tag"
  fi
  chmod +x "$binary_path"
  step "downloaded $binary_path"
fi

# Smoke test the binary so we surface obvious problems before someone
# tries to launch the TUI.
if ! "$binary_path" --version >/dev/null 2>&1; then
  fail "binary at $binary_path failed --version smoke test"
fi
step "binary version: $("$binary_path" --version)"

if (( DOWNLOAD_ONLY == 1 )); then
  step "download-only: skipping symlink"
  exit 0
fi

# Symlink $AM_HOME/am → ~/.local/bin/am, replacing any existing link.
local_bin="$HOME/.local/bin"
mkdir -p "$local_bin"
ln -sf "$AM_HOME/am" "$local_bin/am"
step "symlinked $local_bin/am → $AM_HOME/am"

# PATH check. We deliberately do NOT edit shell rc files; print an
# instruction instead.
case ":$PATH:" in
  *":$local_bin:"*) ;;
  *)
    warn "$local_bin is not on your PATH"
    cat <<EOF

Add this line to your shell rc (~/.zshrc or ~/.bashrc), then restart your shell:

  export PATH="\$HOME/.local/bin:\$PATH"

EOF
    ;;
esac

cat <<EOF

agent-monitor is installed.

Try:
  am help          # subcommand list
  am list          # plain-text dump of active sessions
  am               # launch the dashboard

EOF
