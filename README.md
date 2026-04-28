# agent-monitor

Live dashboard for every Cursor agent session running on your machine. Single-pane TUI plus a tmux-hosted worktree-manager pane — see every agent at once, follow one live, take over a chat in a fresh iTerm tab, spin up new worktrees, all in one keystroke.

**One Go binary, ~30 ms refresh ticks, fully read-only on agent runtime.** Works on macOS (Apple Silicon and Intel).

## Install

One command from a clean shell:

```bash
git clone https://github.com/<owner>/agent-monitor ~/.claude/skills/agent-monitor && \
  ~/.claude/skills/agent-monitor/install.sh
```

Then:

```bash
am          # launch dashboard
am help     # subcommand list
```

The installer:

1. Verifies `tmux`, `ps`, `lsof` are present (warns on missing iTerm2 / `cursor-agent` / `wt`).
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
| `n` | new worktree (modal: branch / base / prompt) |
| `r` | kill agent (and remove worktree if attached) |
| `s` | `wt sync` — rebase onto base |
| `x` | pause / resume the selected agent (`SIGSTOP` / `SIGCONT`) |
| `p` | pause / resume auto-refresh |
| `q` / `D` | detach tmux (dashboard keeps running) |
| `Q` | kill the agent-monitor tmux session |
| `?` | help modal |
| `F2` / `F3`, `M-Tab` | switch panes (session-scoped, never leaks to other tmux sessions) |

## What it watches

Cursor stores transcripts as JSONL under `~/.cursor/projects/<encoded>/agent-transcripts/<chatId>/<chatId>.jsonl`. Each tick the dashboard:

1. Walks the projects dir (one syscall pass).
2. Tails the last ~8 KB of each transcript whose mtime advanced (mtime cache + bounded read).
3. Runs one `ps` and one bulk `lsof` to map chat ids to live `cursor-agent` processes.
4. Classifies each card by **content** (last JSONL entry shape), not just by clock.

This is a fully read-only view of the agent runtime — the dashboard never opens a transcript in full, never touches `cursor-agent`'s sockets, and never blocks on I/O. `cursor-agent` workers detach from their parent terminal, so closing the terminal that started one doesn't kill it; `am` just gives you visibility back from any new terminal.

## Subcommands

```
am                start or attach the dashboard
am attach         attach an existing session (error if none)
am detach         detach current tmux client
am list           plain-text dump (machine-readable)
am follow <id>    open dashboard with bottom pane live-tailing one transcript
am resume <id>    open dashboard with bottom pane running cursor-agent --resume <id>
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
| `AM_PROJECTS_DIR` | `~/.cursor/projects` | where transcripts live |
| `AM_WT_BIN` | `~/.claude/skills/worktrees/scripts/wt` | path to the `wt` CLI |

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
~/.claude/skills/agent-monitor/uninstall.sh
```

Kills the tmux session, removes the `~/.local/bin/am` symlink, and (with confirmation) deletes the install dir. Monitored `cursor-agent` processes and transcripts are never touched.

## License

MIT — see [LICENSE](./LICENSE).
