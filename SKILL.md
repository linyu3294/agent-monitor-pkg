---
name: agent-monitor
description: Zero-impact read-only dashboard for every running Cursor, Claude Code, and Codewhale agent session. Use when the user wants to see what all their agents are doing at once, manage sessions across worktrees, safely close a terminal without killing agents, or continue a background session (follow / resume) from any new terminal. Depends on the `worktrees` skill for all worktree lifecycle operations.
depends_on:
  - worktrees
---

# agent-monitor

Single-pane TUI plus a tmux-hosted worktree-manager pane that stacks every active agent session as a card — Cursor, Claude Code, and Codewhale. Fully read-only — polls filesystem mtimes and tails the last few KB of transcript JSONL. Never opens a transcript in full, never touches the agent runtime, never blocks on I/O.

The dashboard itself is a Go binary at `bin/agent-monitor-darwin-<arch>`. The bash launcher (`am`) handles tmux session lifecycle, the wt-manager pane, and the `follow`/`resume` overrides.

## When to use

Trigger on phrases like:
- "show me all my agents"
- "agent dashboard"
- "monitor my cursor agents"
- "what are all my agents doing"
- "I want to close this terminal without killing the agent"
- "let me continue what that other agent is doing"
- "manage worktrees across sessions"

## Launch

```bash
am          # or `~/.claude/skills/agent-monitor-pkg/am` if not on PATH
```

If a tmux session named `agent-monitor` already exists, `am` attaches to it. Otherwise it spawns one: top pane is the Go dashboard, bottom pane is a `cursor-agent --yolo` agent constrained to only invoke the `wt` CLI (the worktrees-manager agent).

## Architecture

```
am (bash launcher)
  ├─ tmux session 'agent-monitor'
  │   ├─ pane 0.0: bin/agent-monitor (Go binary, the dashboard)
  │   └─ pane 0.1: cursor-agent --yolo (the wt-manager)
  ├─ am follow <id>   → respawns pane 0.1 as a live JSONL tail (Cursor / Claude Code)
  ├─ am resume <id>   → respawns pane 0.1 resuming that session (agent auto-detected)
  ├─ am rebuild       → go build → bin/agent-monitor-darwin-<arch>
  ├─ am restart       → respawn pane 0.0 only
  └─ am update        → git pull + rebuild + restart
```

## Hotkeys

The dashboard footer always shows the most-used set:

```
[j/k] move  [f] follow  [a] attach  [n] new  [i] ide  [x] pause-agent  [r] remove  [?] help
```

Press `?` for the full help modal grouped by category. Hotkeys only fire when the top pane has focus (use `F2`, `F3`, `M-Tab`, or click to focus).

| Group | Key | Action |
|---|---|---|
| Navigation | `j`/`k`/`↓`/`↑` | next / previous |
| | `g`/`G` | first / last |
| | `Enter` | focus selected card in the wt-manager pane |
| Multi-agent | `f` | follow (live transcript tail in iTerm; Cursor and Claude Code transcripts — on a Codewhale card the dashboard points you at `a`) |
| | `a` | resume in new iTerm tab (live-process gate prevents duplicate cards; uses `cursor-agent --resume`, `claude --resume`, or `codewhale resume` to match the card's source) |
| | `y` | copy chat id to clipboard |
| | `o` | open worktree in Cursor |
| | `i` | open nvim + Diffview iTerm tab |
| Worktree lifecycle | `n` | new worktree (modal: repo / branch / base / agent / before / prompt, plus `asks`/`net` only where the selected agent honours them — codewhale gets neither row, claude gets `asks` but no `net`; `agent` is `cursor-agent`, `claude`, or `codewhale`, ←/→ to change; defaults to `codewhale` when it is on `PATH`, then `claude`, then `cursor-agent`. `approval` is the launch posture, ←/→ to change: `on-request` (**default** — the agent asks before acting), `auto` (cursor-agent `--auto-review` / claude `--permission-mode acceptEdits`), `never (unattended)` (cursor-agent `--yolo` / claude `--dangerously-skip-permissions`), or `untrusted (strict)` (the default, since neither CLI has a stricter flag). Codewhale does not take that row: Shift+Tab inside the session cycles Ask → Auto-Review → Full Access, and `--approval-policy` locks that dial so the Auto-Review guardian cannot be left. The dashboard never chooses the posture for you. `net` is egress, ←/→ to change: `on (full access)` (**default**) or `off (workspace-write)` — codewhale is always unsandboxed (`danger-full-access`); claude has no sandbox switch, so the row does not change its command; `on` disables cursor-agent's sandbox (`--sandbox disabled`), so it is a filesystem grant too, and any credential the shell holds becomes reachable. `before` is an optional single-line shell command run in the new worktree before the agent starts; it is joined to the launch line with `&&`, so a failed setup stops the agent from starting at all) |
| | `r` | remove the selected card (y/N confirm). **Worktree card** → kill agent + `wt remove <slug> --force` (worktree dir + branch deleted). **Primary card** → kill agent + drop card (no filesystem changes). |
| | `s` | `wt sync` |
| Agent control | `x` | pause / resume the selected agent (SIGSTOP / SIGCONT) |
| Dashboard | `p` | pause / resume auto-refresh |
| | `e` | show / hide worktrees with no chat (hidden by default) |
| | `q` / `D` | detach tmux |
| | `Q` | kill agent-monitor tmux session |
| | `?` | help modal |
| Pane switching | `F2` / `F3` | top / bottom pane (macOS: needs Fn+F2 or "F-keys as standard") |
| | `C-Up` / `C-Down` | top / bottom pane (recommended fallback — works in every terminal) |
| | `M-Tab` / `M-Shift-Tab` | cycle next / previous (needs iTerm: Option = Esc+; tmux key name is `M-BTab`) |
| | `M-1` / `M-2` | top / bottom pane (needs iTerm: Option = Esc+) |
| | `M-Up` / `M-Down` | top / bottom pane (needs iTerm: Option = Esc+) |
| | `prefix F2` / `prefix F3` | top / bottom pane via tmux prefix (always works) |
| | `prefix Tab` / `prefix BTab` | cycle next / previous via tmux prefix |

All pane-switching bindings live in a session-scoped `agent-monitor-root` key table, so they never leak to other tmux sessions.

### Card model

Each worktree gets exactly one card (the most-recent agent for that worktree wins; older chats on the same worktree are hidden). Worktree cards are pinned — they always surface regardless of `AM_IDLE_THRESHOLD_MIN`, since the worktree itself is the unit of interest. Worktrees with no chat at all get a `(no session)` stub card, hidden by default; press `e` to show them (the header shows how many are hidden). Primary-checkout sessions surface as individual cards and age out at the idle threshold; `r` kills+drops them.

## State classification (the bug fix)

The classifier reads the **last JSONL entry's content shape**, not just its mtime:

- last entry `assistant` + `tool_use` → `ACTIVE` (tool in flight)
- last entry `assistant` + text only → `IDLE` (turn ended, awaiting user) — *was the THINKING-stuck bug*
- last entry `user` → `THINKING` (model is generating)
- no live agent process bound to the session → `IDLE` regardless
- mtime older than 2 min → `IDLE` regardless, except for Codewhale and Claude Code, whose logs are written per turn: a live process with a tool-call tail is waiting on that tool, so it stays `ACTIVE`
- process in `SIGSTOP`ped state → `PAUSED`

## Resume gate (`a` hotkey)

`cursor-agent --resume <id>` against a chat whose `store.db` is already held by another live process **forks a fresh chat** rather than attaching, which used to leak as a duplicate card on the next refresh tick. The Go dashboard now checks `proctable.IsLive(id)` first; if the chat is live, it refuses with:

> session `<id>` is already live in another terminal — focus that terminal, or press 'r' here to kill it first

Use `r` to kill the existing live process before resuming if you really want to take over.

## Performance

| Op (10 cards) | Bash legacy | Go rewrite |
|---|---:|---:|
| Walk projects | ~50 ms | ~3 ms |
| JSONL tail (dirty) | ~30 ms × N | ~0.5 ms × N |
| JSONL tail (clean) | ~30 ms × N | ~0 ms (mtime cache) |
| `ps` + chat→pid map | ~150 ms | ~25 ms |
| Render | ~500 ms | ~3 ms |
| **Total tick (all dirty)** | **~2,500 ms** | **~30 ms** |

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `AM_TMUX_SESSION` | `agent-monitor` | tmux session name |
| `AM_REFRESH_MS` | `2000` | refresh interval |
| `AM_IDLE_THRESHOLD_MIN` | `20` | hide sessions idle longer than this |
| `AM_MAX_CARDS` | `50` | max rendered cards |
| `AM_PROJECTS_DIR` | `~/.cursor/projects` | Cursor transcript store |
| `AM_CLAUDE_DIR` | `~/.claude/projects` | Claude Code transcript store |
| `AM_CODEWHALE_DIR` | `~/.codewhale/sessions` | Codewhale session store |
| `AM_WT_BIN` | `~/.claude/skills/worktrees/scripts/wt` | path to wt CLI |

## Troubleshooting

| Symptom | Remedy |
|---|---|
| `am` says binary not found | Run `am rebuild` (if Go installed) or `./install.sh` to download a prebuilt binary |
| Cards feel stale | Press `p` to confirm auto-refresh is on; check `AM_REFRESH_MS`; verify `--profile` shows <100ms ticks |
| `am list` shows nothing | Increase `AM_IDLE_THRESHOLD_MIN`, or confirm transcripts exist under `~/.cursor/projects/*/agent-transcripts/`, `~/.claude/projects/*/`, or `~/.codewhale/sessions/` |
| `r`/`s`/`o`/`i`/`n` do nothing | The `worktrees` skill is missing or `AM_WT_BIN` is wrong. Reinstall the worktrees skill |
| `a` says "session is already live" | The chat is open in another agent process. Focus that terminal, or press `r` to kill it before resuming |
| a claude card stays `IDLE` while the agent works | Liveness is bound by working directory. The card stays unbound (rather than signal the wrong process) when the process's cwd is not any transcript's `cwd`, when two claude processes share a directory, or when more than one transcript there was written since the process started |
| dashboard pane is dead | Crash-loop protection kicked in. Read the exit message, fix the cause, then `am kill && am` |

## Out of scope

- Remote / multi-machine sessions
- Progress bars per agent (no such signal exists)
- Any write access to transcripts or agent runtime state
- Windows / Linux — macOS + bash + tmux only
