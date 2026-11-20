


<img width="1709" height="1013" alt="Screenshot 2026-07-31 at 11 29 41 AM" src="https://github.com/user-attachments/assets/609617ef-2810-4d68-a6ce-b9c44717d68b" />





# agent-monitor

Live dashboard for every Cursor, Claude Code, and Codewhale agent session running on your machine. Single-pane TUI plus a tmux-hosted worktree-manager pane — see every agent at once, follow one live, take over a chat in a fresh iTerm tab, spin up new worktrees, all in one keystroke.

**One Go binary, ~30 ms refresh ticks, fully read-only on agent runtime.** Works on macOS (Apple Silicon and Intel).

## Install

One command from a clean shell:

```bash
git clone https://github.com/linyu3294/agent-monitor-pkg ~/.claude/skills/agent-monitor-pkg && \
  ~/.claude/skills/agent-monitor-pkg/install.sh
```

The install dir is not special — `install.sh` and `am` self-locate, so you can clone
anywhere (e.g. `~/tools/agent-monitor-pkg`) and it still works.

Then:

```bash
am          # launch dashboard
am help     # subcommand list
```

The installer:

1. Verifies `tmux`, `ps`, `lsof` are present (warns on missing iTerm2 / `cursor-agent` / `claude` / `wt`).
2. Detects your arch (`darwin/arm64` vs `darwin/amd64`).
3. Builds the Go binary if `go` is on `$PATH`; otherwise downloads the matching prebuilt binary from the latest GitHub release.
4. Symlinks `am` into `~/.local/bin/`. **Never edits your shell rc** — if `~/.local/bin` isn't on your `$PATH` you get a one-line copy-pasteable export.

Re-run `install.sh` any time — it's idempotent.

## Hotkeys

| Key | Action |
|---|---|
| `j` / `↓`, `k` / `↑` | next / previous card |
| `g` / `G` | first / last |
| `Enter` | focus selected card in the wt-manager pane |
| `f` | follow (live transcript tail in iTerm) |
| `a` | resume in new iTerm tab (with live-process gate) |
| `y` | copy chat id to clipboard |
| `o` | open worktree in Cursor |
| `i` | open nvim + Diffview iTerm tab |
| `n` | new worktree (modal: repo / branch / base / agent / before / prompt, plus `asks` and `net` where the selected agent honours them) |
| `r` | kill agent (and remove worktree if attached) |
| `s` | sync — rebase the worktree onto its base |
| `x` | pause / resume the selected agent (`SIGSTOP` / `SIGCONT`) |
| `p` | pause / resume auto-refresh |
| `e` | show / hide worktrees with no chat (hidden by default) |
| `q` / `D` | detach tmux (dashboard keeps running) |
| `Q` | kill the agent-monitor tmux session |
| `?` | help modal |
| `F2` / `F3`, `M-Tab` | switch panes (session-scoped, never leaks to other tmux sessions) |

`n` opens a new worktree and launches the chosen CLI in the tab. When `codewhale`
is on `PATH` the agent field defaults to it (then Claude Code, then
`cursor-agent`), so the common case needs no extra keystroke; `←` / `→` cycle
`cursor-agent` → `claude` → `codewhale`.

Selecting `claude` starts Claude Code in the new tab with your prompt
(`claude -- '…'`). It authenticates the way that CLI always does: an
`ANTHROPIC_API_KEY` exported by the tab's shell, or its own stored login. New
tabs start as login shells, so a key exported in `~/.bash_profile` is read by it;
if your shell does not read that file, export the key where it does, or pass it in
the `before:` command.

The modal shows only the rows the selected agent can act on: `asks:` for
cursor-agent and Claude Code, `net:` for cursor-agent alone, and `before:` for
all three. Codewhale gets neither `asks` nor `net` — its dial lives inside the
session and it is always unsandboxed — and Claude Code has no sandbox switch, so
`net:` would change nothing there.

The `asks:` field is the approval posture for **cursor-agent and Claude Code**,
and **it is yours to choose** — the dashboard never switches it for you. It
defaults to `on-request`, so a new agent stops and asks before it acts. `←` / `→`
cycles:

- `on-request` — ask before acting (default)
- `auto` — cursor-agent Smart Auto (`--auto-review`) or Claude Code `--permission-mode acceptEdits`: routine edits run, the rest ask
- `never (unattended)` — run without stopping at all (`--yolo` for cursor-agent, `--dangerously-skip-permissions` for Claude Code), for a worktree you open and walk away from. The modal warns when you select it.
- `untrusted (strict)` — neither CLI has a stricter flag, so this is the default

Codewhale does not take this row. Its permission dial is inside the session:
**Shift+Tab** cycles Ask → Auto-Review → Full Access, which is how you leave
the Auto-Review guardian. Passing `--approval-policy` makes that flag the
controlling source, and Shift+Tab then does nothing. The session starts on
the posture saved in `~/.codewhale/settings.toml`.

| Posture | codewhale | cursor-agent | Claude Code |
|---|---|---|---|
| `on-request` | not pinned; Shift+Tab in the session | default | default |
| `auto` | not pinned; Shift+Tab in the session | `--auto-review` (Smart Auto) | `--permission-mode acceptEdits` |
| `never` | not pinned; Shift+Tab in the session | `--yolo` | `--dangerously-skip-permissions` |
| `untrusted` | not pinned; Shift+Tab in the session | default (no stricter flag exists) | default (no stricter flag exists) |

Codewhale is always started with the sandbox off (`--sandbox-mode danger-full-access`), so its shell is the system shell, the same way `cursor-agent --sandbox disabled` is. When `wt` is installed, `auto` deliberately bypasses `wt create --iterm` — wt can only express `--yolo`, so routing auto through it would silently launch a different posture. Claude Code has no sandbox switch at all: every launch already has the whole filesystem and the network.

## Network posture

`net:` is a second, separate choice — "may you run this?" and "can you reach the
internet?" are different questions, and answering one should not grant the other.
The dashboard starts it at `on`. Set `AM_DEFAULT_NET=off` to leave cursor-agent on its configured default. It stays a per-launch choice, like `never (unattended)`.

Codewhale has no separate network switch. Its only unsandboxed mode is `danger-full-access`, so every codewhale launch uses that, whichever way `net:` is set. Claude Code has no sandbox switch either, so `net:` does not change its command line. `net:` changes cursor-agent only:

| `net:` | codewhale | cursor-agent | Claude Code |
|---|---|---|---|
| `off (workspace-write)` | `--sandbox-mode danger-full-access` | no flag (its configured default) | no flag (always unsandboxed) |
| `on (full access)` | `--sandbox-mode danger-full-access` | `--sandbox disabled` | no flag (always unsandboxed) |

`danger-full-access` and `--sandbox disabled` both turn the sandbox off, so the agent gets the system shell and the whole filesystem. Any credential that shell already holds — an AWS session from `aws-login`, an SSH agent, a token in the environment — is reachable by the agent.

When `net:` is `off`, cursor-agent gets no sandbox flag at all. Its effective
default is whatever its own config says, and adding `--sandbox enabled` would
change behaviour for people who never asked for it.

## Running a command before the agent

The `before:` field is an optional shell command run in the new worktree, in the
same tab, immediately before the agent starts. It exists for setup the agent must
inherit — an SSO login, `direnv allow`, `nvm use`, `npm ci` — because a command run
in some other terminal is invisible to the agent's shell.

The tab is composed as one line. When `before` is set, bash first sources
`~/.bash_profile` in that same shell, so a function or alias defined there
(`aws-login`, for example) exists, and an `export` it makes is inherited by the
agent. A non-login interactive bash does not read `.bash_profile` on its own.

```sh
cd '<worktree>' && . ~/.bash_profile && <before> && <agent command>
```

`&&`, not `;`, on purpose: if the setup command fails, the agent does not start and
the tab stops at the prompt with the error still on screen. A half-prepared
environment never gets an agent attached to it.

A non-empty `before` bypasses the `wt` fast path for the same reason `auto` and
`net: on` do: `wt create --iterm` accepts only `--yolo` and `--prompt`, so routing
through it would silently drop the setup command.

The field is single-line — `textinput` replaces newlines and tabs with spaces, so a
multi-line paste is joined into one command rather than run as several. `before` is
not remembered between launches; the modal opens with it empty.

`before: aws-login bora` on its own does **not** give the agent working AWS access:
the credentials land in the tab's shell, but the sandbox still blocks egress. For
that combination you need `net: on` as well — and everything the warning above says
about widening access then applies.

## What it watches

**Cursor.** Transcripts are JSONL under `~/.cursor/projects/<encoded>/agent-transcripts/<chatId>/<chatId>.jsonl`.

**Codewhale.** Session logs are JSONL under `~/.codewhale/sessions/<uuid>.json` (override the directory with `AM_CODEWHALE_DIR`).

**Claude Code.** Transcripts are JSONL under `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl` (override the directory with `AM_CLAUDE_DIR`).

Each tick the dashboard:

1. Walks all three stores (one syscall pass each).
2. Reads a bounded head + tail of each log whose mtime advanced (mtime cache; Cursor: 8 KB tail, Codewhale: 512 KB head for the workspace and 64 KB tail for the last turn, Claude Code: 64 KB head for the workspace and a backwards-growing tail for the last turn).
3. Runs one `ps` and one bulk `lsof` to map sessions to live processes: Cursor through its open `store.db`, Codewhale through its open session log, Claude Code through the process's working directory — it holds no session file open.
4. Classifies each card by **content** (last JSONL entry shape), not just by clock.

The workspace for a Codewhale card is read from the log's own metadata when it states one, and otherwise inferred from any absolute path in the log that exists on disk — preferring a git checkout. A card whose workspace can't be established shows `(unknown workspace)` rather than a guess. A Claude card's workspace is the `cwd` every transcript record states, so it follows the directory the CLI was actually launched in, and its sidechain (subagent) records are skipped so the card reports the thread you are watching.

Claude Code keeps no file descriptor tied to a session, so liveness is bound by workspace: a `claude` process whose working directory matches the session's workspace is that session. If a process does hold its own transcript open, that file names the session exactly and wins over the workspace guess. Otherwise the join only accepts a transcript written at or after the process started, and refuses when more than one qualifies — two live agents in one directory, or a finished batch session alongside the live one. An unbound card loses its `ACTIVE` state; `x` and `r` can never signal a process the card does not name.

This is a fully read-only view of the agent runtime — the dashboard never opens a log in full, never touches an agent's sockets, and never blocks on I/O. Agents detach from their parent terminal, so closing the terminal that started one doesn't kill it; `am` just gives you visibility back from any new terminal.

Every card names its agent (`cursor`, `claude`, or `codewhale`) next to the
project name, so a mixed dashboard stays readable at a glance.

A card's `cmd:` row names the CLI that resumes it (`cursor-agent --resume <id>`, `claude --resume <id>`, or `codewhale resume <id>`), and `am list` reports that `SOURCE` per row. `f` (follow) works for Cursor and Claude Code transcripts; on a Codewhale card it points you at `a` instead of tailing the wrong file.

**Removal never deletes context.** `r` signals the agent and, for a worktree card, deletes the worktree directory and its branch — including any uncommitted work in it. The conversation itself is untouched for every agent type: Cursor's transcript stays under `~/.cursor/projects/...`, Claude Code's under `~/.claude/projects/...`, and Codewhale's log under `~/.codewhale/sessions/`, so `<cli> resume <id>` still replays the full history. The one thing removal destroys is the workspace, so resuming into a deleted worktree fails even though the transcript is intact.

Once a worktree is gone its card stays gone — not just until the next restart. A card whose worktree no longer exists has nothing to act on, and tombstone hiding lives only in memory. The exception is an agent still running in a deleted worktree: it keeps its card so you can kill it.

## Subcommands

```
am                start or attach the dashboard
am attach         attach an existing session (error if none)
am detach         detach current tmux client
am list           plain-text dump (machine-readable, one SOURCE column per agent)
am follow <id>    open dashboard with bottom pane live-tailing one transcript (Cursor / Claude Code)
am resume <id>    open dashboard with bottom pane resuming that session (agent auto-detected)
am kill           kill the agent-monitor tmux session (monitored agents unaffected)
am rebuild        rebuild the Go binary (requires go on PATH)
am restart        bounce the dashboard pane (leave wt-manager untouched)
am update         git pull + rebuild + restart
am help           subcommand list + env vars
```

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `AM_TMUX_SESSION` | `agent-monitor` | tmux session name |
| `AM_REFRESH_MS` | `2000` | dashboard tick interval |
| `AM_IDLE_THRESHOLD_MIN` | `20` | hide sessions idle longer than this |
| `AM_MAX_CARDS` | `50` | upper bound on cards |
| `AM_PROJECTS_DIR` | `~/.cursor/projects` | Cursor transcript store |
| `AM_CLAUDE_DIR` | `~/.claude/projects` | Claude Code transcript store |
| `AM_CODEWHALE_DIR` | `~/.codewhale/sessions` | Codewhale session store |
| `AM_WT_BIN` | `~/.claude/skills/worktrees/scripts/wt` | path to the `wt` CLI |
| `AM_DEFAULT_APPROVAL` | `on-request` | initial `asks:` value in the `n` modal: `on-request`, `auto`, `never`, `untrusted` |
| `AM_DEFAULT_NET` | `on` | initial `net:` value: `off` or `on` (full access) |
| `AM_DEFAULT_BEFORE` | unset | initial `before:` command, e.g. `aws-login bora` |

The three `AM_DEFAULT_*` vars only decide where the `n` modal's cursor starts.
They are still editable per launch, and the modal still shows the egress warning
when `net:` resolves to `on` — a default is not a silent grant. `before` is
re-applied every time the modal opens, so clearing it for one launch does not
discard the configured default for the next.

An unparseable `AM_DEFAULT_APPROVAL` or `AM_DEFAULT_NET` prints a warning to
stderr and falls back to the default rather than refusing to start. Set them once
in your shell profile if you always want the same starting point:

```sh
export AM_DEFAULT_NET=on
export AM_DEFAULT_BEFORE='aws-login bora'
export AM_DEFAULT_APPROVAL=on-request
```

With those set, `n` → `Enter` on a repo is enough; `net:` starts at `on (full
access)` and `before:` already holds the login command.

Remember that `net: on` disables the sandbox for the launched agent. If you only
want the login command without granting egress, set `AM_DEFAULT_BEFORE` alone —
but note that the login alone will not give the agent working AWS access, because
the sandbox still blocks the API calls.

## Performance

| Op (10 cards) | Bash legacy | Go rewrite |
|---|---:|---:|
| Walk projects dir | ~50 ms | ~3 ms |
| JSONL tail (dirty) | ~30 ms × N | ~0.5 ms × N |
| `ps` + chat→pid map | ~150 ms | ~25 ms |
| Render | ~500 ms | ~3 ms |
| **Total tick (all dirty)** | **~2,500 ms** | **~30 ms** |
| **Total tick (none dirty)** | **~2,500 ms** | **~5 ms** |

## Bugs fixed vs. the legacy bash implementation

- **Stuck on `THINKING` after every reply** — old classifier was mtime-only and labeled finished turns as `THINKING` for ~2 min. New classifier reads the last JSONL entry's role + tool_use shape.
- **`a` resume created a duplicate card** — `cursor-agent --resume <id>` against a chat whose `store.db` is already held forks a fresh chat. New behavior: refuses with a status bar message naming the live terminal.
- **Hardcoded username/path prefix** — old `Users-rebet-Documents-` strip; now derived from `os.UserHomeDir()`.
- **State lag from refresh-blocking** — old loop forked 100+ subshells per tick, blocking key handling. Go binary refreshes in <30 ms while bubbletea handles keys at ~60 fps.
- **Stale code in long-lived tmux pane** — bash sources files once. Go binary requires only `am rebuild && am restart`.

## Uninstall

```bash
~/.claude/skills/agent-monitor-pkg/uninstall.sh
```

Kills the tmux session, removes the `~/.local/bin/am` symlink, and (with confirmation) deletes the install dir. Monitored `cursor-agent`, `claude`, and `codewhale` processes and their transcripts are never touched.

## License

MIT — see [LICENSE](./LICENSE).
