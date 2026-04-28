package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textinput"

	"github.com/agent-monitor/agent-monitor/internal/scanner"
	"github.com/agent-monitor/agent-monitor/internal/session"
)

// handleKey routes a keypress based on the current modal state.
func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.state {
	case stateHelp:
		// Any key closes the help modal.
		m.state = stateList
		return m, nil
	case stateNewWt:
		return m.handleNewWtKey(msg)
	case stateConfirmRemove:
		return m.handleConfirmRemoveKey(msg)
	}
	return m.handleListKey(msg)
}

// handleListKey covers the default (card list) state.
func (m *model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "Q":
		// Q must actually destroy the tmux session — otherwise the
		// `am` supervisor loop immediately respawns the dashboard
		// after tea.Quit and nothing visible happens. We kill the
		// session first (which also kills the supervisor's bash),
		// then quit the binary as a fallback for non-tmux invocations.
		// Monitored cursor-agent worker-servers run in *other*
		// processes and are untouched.
		sess := os.Getenv("AM_TMUX_SESSION")
		if sess == "" {
			sess = "agent-monitor"
		}
		_ = exec.Command("tmux", "kill-session", "-t", sess).Run()
		return m, tea.Quit
	case "ctrl+c":
		return m, tea.Quit
	case "q", "D":
		// Detach tmux, leave the dashboard running. We don't try to be
		// clever — if not in tmux, just exit.
		_ = exec.Command("tmux", "detach-client").Run()
		return m, nil
	case "j", "down":
		if len(m.sessions) > 0 && m.selected < len(m.sessions)-1 {
			m.selected++
		}
		return m, nil
	case "k", "up":
		if m.selected > 0 {
			m.selected--
		}
		return m, nil
	case "g":
		m.selected = 0
		return m, nil
	case "G":
		if len(m.sessions) > 0 {
			m.selected = len(m.sessions) - 1
		}
		return m, nil
	case "?":
		m.state = stateHelp
		return m, nil
	case "p":
		m.paused = !m.paused
		title := "auto-refresh paused"
		if !m.paused {
			title = "auto-refresh resumed"
		}
		return m, m.flashConfirm(title, "")
	case "r":
		return m.startRemove()
	case "y":
		s := m.selectedSession()
		if s == nil {
			return m, nil
		}
		_ = pbcopy(s.ID)
		return m, m.flashConfirm("copied chat id to clipboard", s.ID)
	case "a":
		return m.handleResume()
	case "f":
		return m.handleFollow()
	case "x":
		return m.handlePauseToggle()
	case "n":
		return m.openNewWt()
	case "i":
		return m.handleIDE()
	case "o":
		return m.handleOpen()
	case "s":
		return m.handleSync()
	case "enter":
		return m.handleEnter()
	}
	return m, nil
}

// handleResume gates the `a` hotkey on live-process state — the central
// fix for the "resume creates a new card" bug.
func (m *model) handleResume() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	if m.proc.IsLive(s.ID) {
		return m, m.flashConfirm(
			fmt.Sprintf("session %s is already live in another terminal", short(s.ID)),
			"focus that terminal, or press 'r' here to kill it first",
		)
	}
	cwd := m.resolveRepoPath(s)
	if cwd == "" {
		return m, m.flashConfirm("can't resolve workspace path", "")
	}
	id := s.ID
	return m, m.runWtCmd(
		fmt.Sprintf("resumed %s in new iTerm tab", short(id)),
		func() error { return m.wt.Resume(id, cwd) },
	)
}

// handleFollow runs `am follow <id>` in a new iTerm tab. We delegate to
// the launcher rather than re-implement, since `am follow` already has
// its own JSONL pretty-tail logic.
func (m *model) handleFollow() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	id := s.ID
	return m, m.runWtCmd(
		fmt.Sprintf("following %s in new iTerm tab", short(id)),
		func() error {
			// Reuse iTerm-tab spawning by going through `wt resume`'s
			// cwd machinery — but for follow we want a tail loop, not
			// `cursor-agent --resume`. The simplest portable path:
			// open iTerm with `am follow <id>`.
			am, err := exec.LookPath("am")
			if err != nil {
				return err
			}
			return openIterm("", fmt.Sprintf("%s follow %s", am, id))
		},
	)
}

// handlePauseToggle toggles SIGSTOP/SIGCONT on the selected chat's
// user-facing process.
func (m *model) handlePauseToggle() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	if !m.proc.IsLive(s.ID) {
		return m, m.flashConfirm("no live cursor-agent process for this chat", "(already exited?)")
	}
	id := s.ID
	wasPaused := m.proc.IsPaused(id)
	if err := m.proc.TogglePause(id); err != nil {
		return m, m.flashConfirm("pause toggle failed", err.Error())
	}
	verb := "paused"
	if wasPaused {
		verb = "resumed"
	}
	return m, tea.Batch(
		m.flashConfirm(fmt.Sprintf("%s %s", verb, short(id)), ""),
		m.refreshCmd(),
	)
}

// handleIDE opens an nvim+Diffview iTerm tab for a worktree-backed card.
func (m *model) handleIDE() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	if s.WorktreeSlug == "" {
		return m, m.flashConfirm("no worktree attached to this session", "(IDE tab is worktree-only)")
	}
	primary := scanner.DecodeProjectPath(s.ProjectDir)
	slug := s.WorktreeSlug
	if real := scanner.SlugFromEncoded(primary, slug); real != "" {
		slug = real
	}
	repo := primary
	return m, m.runWtCmd(
		fmt.Sprintf("opened IDE for %s", slug),
		func() error { return m.wt.IDE(repo, slug) },
	)
}

// handleOpen opens the worktree (or primary) in Cursor.
func (m *model) handleOpen() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	primary := scanner.DecodeProjectPath(s.ProjectDir)
	slug := s.WorktreeSlug
	if slug != "" {
		if real := scanner.SlugFromEncoded(primary, slug); real != "" {
			slug = real
		}
	}
	repo := primary
	return m, m.runWtCmd(
		fmt.Sprintf("opening %s in Cursor", repo),
		func() error {
			if slug != "" {
				return m.wt.Open(repo, slug)
			}
			// No slug → fall back to the cursor CLI on the primary.
			return exec.Command("cursor", repo).Start()
		},
	)
}

// handleSync runs `wt sync` for the worktree.
func (m *model) handleSync() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	primary := scanner.DecodeProjectPath(s.ProjectDir)
	repo := primary
	if s.WorktreeSlug != "" {
		if p, err := m.wt.PathFor(primary, s.WorktreeSlug); err == nil && p != "" {
			repo = p
		}
	}
	return m, m.runWtCmd("wt sync done", func() error { return m.wt.Sync(repo) })
}

// handleEnter focuses the selected card in the wt-manager bottom pane
// by sending a tmux send-keys message naming the chat id and slug.
// Useful for "now act on this session" workflows.
func (m *model) handleEnter() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	msg := fmt.Sprintf("focus session %s", s.ID)
	if s.WorktreeSlug != "" {
		msg = fmt.Sprintf("focus worktree %s (session %s)", s.WorktreeSlug, s.ID)
	}
	if err := tmuxSendKeys(":0.1", msg); err != nil {
		return m, m.flashConfirm("not in agent-monitor tmux session", "Enter only works inside `am`")
	}
	return m, m.flashConfirm("focused in wt-manager", short(s.ID))
}

// startRemove pops the y/N confirm bar for the `r` hotkey.
//
// Removal policy:
//   - Worktree card: kill the agent AND `wt remove <slug> --force` (drops
//     the worktree dir + branch). y/N confirm because the branch deletion
//     is destructive — work in progress on that branch will be gone.
//   - Primary card: kill the agent (SIGTERM, swallowed if already idle)
//     and tombstone the card so it disappears on the next refresh. No
//     filesystem changes — primary checkout stays.
func (m *model) startRemove() (tea.Model, tea.Cmd) {
	s := m.selectedSession()
	if s == nil {
		return m, nil
	}
	m.state = stateConfirmRemove
	if s.WorktreeSlug != "" {
		slug := s.WorktreeSlug
		if primary := scanner.DecodeProjectPath(s.ProjectDir); primary != "" {
			if real := scanner.SlugFromEncoded(primary, slug); real != "" {
				slug = real
			}
		}
		m.confirm = confirmBar{
			title:    fmt.Sprintf("Kill agent %s and remove worktree '%s'? (branch will be deleted)", short(s.ID), slug),
			subtitle: "y=yes   any other key=no",
		}
		return m, nil
	}
	m.confirm = confirmBar{
		title:    fmt.Sprintf("Kill agent %s? (primary checkout — agent terminated, card dropped)", short(s.ID)),
		subtitle: "y=yes   any other key=no",
	}
	return m, nil
}

// handleConfirmRemoveKey routes keys while the y/N confirm is up.
//
// Reachable for both card types (worktree and primary). Worktree cards
// trigger an additional `wt remove --force` after the kill so the
// worktree directory and branch are deleted alongside the agent.
func (m *model) handleConfirmRemoveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	m.state = stateList
	if key != "y" && key != "Y" {
		m.confirm = confirmBar{}
		return m, nil
	}
	s := m.selectedSession()
	if s == nil {
		m.confirm = confirmBar{}
		return m, nil
	}
	id := s.ID
	hasWorktree := s.WorktreeSlug != ""
	primary := scanner.DecodeProjectPath(s.ProjectDir)
	slug := s.WorktreeSlug
	if hasWorktree && primary != "" {
		if real := scanner.SlugFromEncoded(primary, slug); real != "" {
			slug = real
		}
	}

	// Tombstone the chat id either way so the card disappears on the
	// next refresh tick. For worktree cards `wt remove` will also drop
	// the project dir on disk and the next refresh would naturally
	// stop surfacing it, but the tombstone removes the visual lag
	// between the keypress and the next scan.
	m.tombstones[id] = struct{}{}

	label := fmt.Sprintf("killed agent %s", short(id))
	if hasWorktree {
		label = fmt.Sprintf("removed worktree '%s' (agent %s killed)", slug, short(id))
	}
	return m, tea.Batch(
		m.runWtCmd(
			label,
			func() error {
				// proctable.Kill returns an error if the chat had no
				// live process to begin with (idle agent). That's the
				// expected case for "remove an idle card" — swallow
				// it so the worktree-removal path still runs.
				_ = m.proc.Kill(id)
				if hasWorktree {
					if primary == "" {
						return fmt.Errorf("could not infer primary repo for worktree '%s'", slug)
					}
					return m.wt.Remove(primary, slug, true)
				}
				return nil
			},
		),
		m.refreshCmd(),
	)
}

// openNewWt enters the new-worktree modal.
func (m *model) openNewWt() (tea.Model, tea.Cmd) {
	m.state = stateNewWt
	m.form.branch.SetValue("")
	m.form.prompt.SetValue("")
	m.form.focused = 0
	m.form.branch.Focus()
	m.form.base.Blur()
	m.form.prompt.Blur()
	return m, textinput.Blink
}

// handleNewWtKey routes keys while the new-worktree modal is up.
func (m *model) handleNewWtKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc", "ctrl+c":
		m.state = stateList
		return m, nil
	case "tab", "down":
		m.cycleNewWtFocus(+1)
		return m, nil
	case "shift+tab", "up":
		m.cycleNewWtFocus(-1)
		return m, nil
	case "enter":
		branch := strings.TrimSpace(m.form.branch.Value())
		base := strings.TrimSpace(m.form.base.Value())
		prompt := m.form.prompt.Value()
		if branch == "" {
			m.state = stateList
			return m, m.flashConfirm("new worktree cancelled", "(empty branch)")
		}
		if base == "" {
			base = "origin/main"
		}
		s := m.selectedSession()
		repo := ""
		if s != nil {
			if p := scanner.DecodeProjectPath(s.ProjectDir); p != "" {
				repo = p
			}
		}
		if repo == "" {
			m.state = stateList
			return m, m.flashConfirm("new worktree failed", "no repo could be inferred")
		}
		m.state = stateList
		return m, m.runWtCmd(
			fmt.Sprintf("created worktree %s", branch),
			func() error { return m.wt.Create(repo, branch, base, prompt) },
		)
	}
	// Pass to the focused field.
	var cmd tea.Cmd
	switch m.form.focused {
	case 0:
		m.form.branch, cmd = m.form.branch.Update(msg)
	case 1:
		m.form.base, cmd = m.form.base.Update(msg)
	case 2:
		m.form.prompt, cmd = m.form.prompt.Update(msg)
	}
	return m, cmd
}

func (m *model) cycleNewWtFocus(delta int) {
	m.form.focused = (m.form.focused + delta + 3) % 3
	m.form.branch.Blur()
	m.form.base.Blur()
	m.form.prompt.Blur()
	switch m.form.focused {
	case 0:
		m.form.branch.Focus()
	case 1:
		m.form.base.Focus()
	case 2:
		m.form.prompt.Focus()
	}
}

// short returns the first 8 chars of a UUID for compact status messages.
func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// tmuxSendKeys sends a literal string + Enter to <target>. Returns an
// error if tmux exits non-zero (most commonly: not in a tmux session).
func tmuxSendKeys(target, text string) error {
	if err := exec.Command("tmux", "send-keys", "-t", target, text, "Enter").Run(); err != nil {
		return err
	}
	return nil
}

// openIterm spawns a new iTerm2 tab cd'ing into <cwd> and running <cmd>.
// Best-effort — returns an AppleScript error verbatim if iTerm refuses.
func openIterm(cwd, cmd string) error {
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

// Compile-time safety: ensure the session import is used (some build
// flag combinations strip references to types we only touch indirectly
// via the classifier).
var _ = session.StateActive
