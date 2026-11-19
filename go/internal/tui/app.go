// Package tui is the bubbletea front-end for agent-monitor.
//
// One model (`model`) drives every view: card list, help modal,
// new-worktree modal, and the bottom confirm bar. State is reactive —
// every key event produces a new model + optional command (subprocess
// dispatch, refresh tick), and bubbletea diffs the view to the
// terminal.
//
// All hot keys live in this package; see keys.go for the binding table.
// All visual styling lives in view.go via lipgloss.
package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/agent-monitor/agent-monitor/internal/classifier"
	"github.com/agent-monitor/agent-monitor/internal/proctable"
	"github.com/agent-monitor/agent-monitor/internal/scanner"
	"github.com/agent-monitor/agent-monitor/internal/session"
	"github.com/agent-monitor/agent-monitor/internal/wtbridge"
)

// Options configures the dashboard model.
type Options struct {
	RefreshInterval time.Duration
	IdleThreshold   time.Duration
	MaxCards        int

	// New-worktree defaults. They seed the `n` modal and are still fully
	// editable per launch — they decide where the cursor starts, not what the
	// launch is allowed to do. Zero values are the confined, asks-first
	// defaults, so an unset field cannot widen a launch.
	DefaultApproval wtbridge.Approval
	DefaultNetwork  wtbridge.Network
	DefaultBefore   string
}

// uiState enumerates the dashboard's modal layers.
type uiState int

const (
	stateList uiState = iota
	stateHelp
	stateNewWt
	stateConfirmRemove
)

// confirmBar holds a transient one/two-line message under the footer.
// Cleared by a tea.tick after `expiresAt`.
type confirmBar struct {
	title    string
	subtitle string
	expires  time.Time
}

// newWtForm holds the in-progress fields of the `n` modal.
type newWtForm struct {
	repo     textinput.Model
	branch   textinput.Model
	base     textinput.Model
	prompt   textinput.Model
	before   textinput.Model
	agent    wtbridge.Agent
	approval wtbridge.Approval
	network  wtbridge.Network
	focused  int // 0=repo, 1=branch, 2=base, 3=agent, 4=approval, 5=network, 6=before, 7=prompt
	err      string
}

// model is the bubbletea Model for the whole dashboard.
type model struct {
	opts Options

	scanner *scanner.Scanner
	proc    *proctable.Table
	wt      *wtbridge.Bridge

	sessions     []session.Session
	selected     int
	scrollOffset int  // index of the topmost card rendered in the viewport
	paused       bool // dashboard auto-refresh paused (the `p` hotkey)
	showEmpty    bool // show stub cards for worktrees with no chat (the `e` hotkey)
	hiddenEmpty  int  // stub cards filtered out on the last refresh

	state      uiState
	confirm    confirmBar
	form       newWtForm
	tombstones map[string]struct{} // cards removed via `r`, keyed by tombstoneKey

	width  int
	height int

	help bool

	// Tombstone for the dashboard's own wt-manager chat id; filled at
	// every refresh from proctable.
	wtManagerID string

	version string
}

// tickMsg is delivered every RefreshInterval to trigger a rescan.
type tickMsg time.Time

// scanResultMsg is delivered after a synchronous scan completes (we
// keep the scan in-process; bubbletea doesn't require Cmd dispatch for
// fast work).
type scanResultMsg struct {
	sessions    []session.Session
	wtManagerID string
	hiddenEmpty int
}

// confirmExpireMsg clears the bottom bar.
type confirmExpireMsg time.Time

// wtResultMsg surfaces the outcome of a `wt`/process action.
type wtResultMsg struct {
	title    string
	subtitle string
	failed   bool
	// restore, when set on a failed result, is a tombstone key to lift
	// so the card reappears instead of silently hiding a live worktree.
	restore string
}

// New builds an initial model.
func New(opts Options, version string) *model {
	scn := scanner.New(scanner.Options{
		IdleThreshold: opts.IdleThreshold,
		MaxCards:      opts.MaxCards,
	})
	repo := textinput.New()
	repo.Prompt = ""
	repo.Placeholder = "path to git repo"
	repo.CharLimit = 512
	repo.Width = 50
	branch := textinput.New()
	branch.Prompt = ""
	branch.CharLimit = 80
	branch.Width = 50
	base := textinput.New()
	base.Prompt = ""
	base.Placeholder = "detected from the repo"
	base.CharLimit = 80
	base.Width = 50
	prompt := textinput.New()
	prompt.Prompt = ""
	prompt.CharLimit = 200
	prompt.Width = 50
	before := textinput.New()
	before.Prompt = ""
	before.Placeholder = "optional: shell command to run first"
	before.CharLimit = 300
	before.Width = 50
	return &model{
		opts:    opts,
		scanner: scn,
		proc:    proctable.New(),
		wt:      wtbridge.New(),
		state:   stateList,
		form: newWtForm{
			repo:   repo,
			branch: branch,
			base:   base,
			prompt: prompt,
			before: before,
			agent:  defaultAgent(),
			// From Options, which main.go fills from AM_DEFAULT_*. The zero
			// values are on-request and confined: a new agent asks before it
			// acts and has no egress unless the operator configured otherwise
			// or the person launching it changes the field.
			approval: opts.DefaultApproval,
			network:  opts.DefaultNetwork,
		},
		tombstones: map[string]struct{}{},
		version:    version,
	}
}

// defaultAgent prefers codewhale when it is installed, then Claude Code,
// so the `n` modal launches the lowest-friction agent without an extra
// keystroke. Cursor stays the fallback on machines that have neither.
func defaultAgent() wtbridge.Agent {
	if wtbridge.AgentCodewhale.Available() {
		return wtbridge.AgentCodewhale
	}
	if wtbridge.AgentClaude.Available() {
		return wtbridge.AgentClaude
	}
	return wtbridge.AgentCursor
}

// Init is bubbletea's startup hook; we kick off the first tick and
// render before any key arrives.
func (m *model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), m.scheduleTick())
}

// scheduleTick fires the next periodic refresh.
func (m *model) scheduleTick() tea.Cmd {
	return tea.Tick(m.opts.RefreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// refreshCmd does an in-process scan + proctable refresh and returns the result.
//
// Card model:
//   - Worktree-backed sessions collapse to one card per (encoded project dir).
//     Cursor encodes the worktree path into the project dir, so two transcripts
//     for the same worktree share the same ProjectDir; we keep only the
//     most-recent one (scanner returns sessions sorted newest first, so the
//     first one wins). A real session always beats a `(no session)` stub for
//     the same worktree: the stub has no id to resume.
//   - Primary-checkout sessions surface as individual cards — the user can
//     run multiple agents against the main checkout and remove them
//     independently with `r`.
func (m *model) refreshCmd() tea.Cmd {
	// Snapshot on the Update goroutine: the Cmd runs concurrently with
	// Update, which mutates m.tombstones.
	tombstones := make(map[string]struct{}, len(m.tombstones))
	for k := range m.tombstones {
		tombstones[k] = struct{}{}
	}
	showEmpty := m.showEmpty
	return func() tea.Msg {
		m.proc.Refresh()
		raw := m.scanner.Scan()
		// Claude Code holds no session file descriptor, so its processes
		// bind to a card through the workspace they run in. That join
		// needs the scan's workspace → session mapping, which is why it
		// happens here and not inside Refresh.
		m.proc.BindClaudeSessions(proctable.ClaudeWorkspaceIndex(raw))
		var wtmID string
		hiddenEmpty := 0
		filtered := raw[:0]
		seenWorktree := map[string]int{}
		for _, s := range raw {
			if _, ok := tombstones[tombstoneKey(s)]; ok {
				continue
			}
			if s.ID == scanner.NoSessionID && !showEmpty {
				hiddenEmpty++
				continue
			}
			if m.proc.IsWtManager(s.ID) {
				wtmID = s.ID
				continue
			}
			// A worktree that no longer exists has nothing to act on. Drop
			// it so `r` stays effective across restarts — tombstones live
			// only in memory. A still-running agent is kept, because a
			// deleted workspace is exactly when you want a way to kill it.
			if OrphanedWorktree(s, m.proc.IsLive(s.ID)) {
				continue
			}
			s.State = classifier.Classify(s, m.proc)
			s.PID = m.proc.PID(s.ID)
			s.Paused = m.proc.IsPaused(s.ID)
			if s.WorktreeSlug != "" {
				if idx, ok := seenWorktree[s.ProjectDir]; ok {
					// A worktree's directory mtime can overtake its
					// transcript's, which would leave the stub card in
					// front of a real session. Replace it in place: the
					// card must be the one that can be resumed.
					if filtered[idx].ID == scanner.NoSessionID && s.ID != scanner.NoSessionID {
						filtered[idx] = s
					}
					continue
				}
				seenWorktree[s.ProjectDir] = len(filtered)
			}
			filtered = append(filtered, s)
		}
		return scanResultMsg{sessions: filtered, wtManagerID: wtmID, hiddenEmpty: hiddenEmpty}
	}
}

// Update is the central reducer.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		if m.paused {
			return m, m.scheduleTick()
		}
		return m, tea.Batch(m.refreshCmd(), m.scheduleTick())

	case scanResultMsg:
		m.sessions = msg.sessions
		m.wtManagerID = msg.wtManagerID
		m.hiddenEmpty = msg.hiddenEmpty
		if m.selected >= len(m.sessions) {
			m.selected = len(m.sessions) - 1
		}
		if m.selected < 0 {
			m.selected = 0
		}
		// Clamp the scroll offset too — sessions may have just been
		// removed (tombstone, idle filter, dedup) so an offset that
		// was valid last tick may now point past the end.
		if m.scrollOffset > m.selected {
			m.scrollOffset = m.selected
		}
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
		return m, nil

	case confirmExpireMsg:
		m.confirm = confirmBar{}
		return m, nil

	case wtResultMsg:
		if msg.failed && msg.restore != "" {
			delete(m.tombstones, msg.restore)
			return m, tea.Batch(m.flashConfirm(msg.title, msg.subtitle), m.refreshCmd())
		}
		return m, m.flashConfirm(msg.title, msg.subtitle)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// flashConfirm shows a transient bar for ~1.4s.
func (m *model) flashConfirm(title, subtitle string) tea.Cmd {
	m.confirm = confirmBar{
		title:    title,
		subtitle: subtitle,
		expires:  time.Now().Add(1400 * time.Millisecond),
	}
	return tea.Tick(1400*time.Millisecond, func(t time.Time) tea.Msg { return confirmExpireMsg(t) })
}

// selectedSession returns a pointer to the currently selected session
// or nil if there is none.
func (m *model) selectedSession() *session.Session {
	if len(m.sessions) == 0 || m.selected < 0 || m.selected >= len(m.sessions) {
		return nil
	}
	return &m.sessions[m.selected]
}

// agentRuntimeDirs are path segments belonging to the agent CLIs
// themselves. A transcript's `cd` can land in one of these (a real card
// recorded ~/.local/share/cursor-agent/versions/<v> as its cwd), and no
// agent session's workspace is ever inside its own install tree.
var agentRuntimeDirs = []string{
	"/.local/share/cursor-agent/",
	"/.cursor/",
	"/.codewhale/",
	"/.claude/",
}

func looksLikeAgentRuntimePath(p string) bool {
	withSlash := p + "/"
	for _, dir := range agentRuntimeDirs {
		if strings.Contains(withSlash, dir) {
			return true
		}
	}
	return false
}

// sessionWorkspace returns the directory the agent actually ran in, or
// "" when it can't be established.
//
// Precedence is deliberate, and the order matters:
//
//  1. For a worktree session the encoded project dir is authoritative —
//     Cursor encoded the workspace it opened, and resolving the real slug
//     against `.worktrees/` recovers the exact path.
//  2. CWDHint is only a shell-command observation. A transcript's `cd`
//     can point at anything the agent touched, including its own install
//     directory; a real card had CWDHint = ~/.local/share/cursor-agent/
//     versions/<v>, and trusting it made removal hunt for
//     `.worktrees/<slug>` inside the Cursor binary directory.
//  3. Decoded project dir last, for primary sessions whose ProjectDir is
//     a parent folder rather than a repo.
func sessionWorkspace(s *session.Session) string {
	if s == nil {
		return ""
	}
	primary := scanner.DecodeProjectPath(s.ProjectDir)
	if s.WorktreeSlug != "" && primary != "" {
		if real := scanner.SlugFromEncoded(primary, s.WorktreeSlug); real != "" {
			wt := filepath.Join(primary, ".worktrees", real)
			if st, err := os.Stat(wt); err == nil && st.IsDir() {
				return wt
			}
		}
		return primary
	}
	if s.CWDHint != "" && !looksLikeAgentRuntimePath(s.CWDHint) {
		if st, err := os.Stat(s.CWDHint); err == nil && st.IsDir() {
			return s.CWDHint
		}
	}
	return primary
}

// sessionPrimary returns the primary git checkout that owns the session,
// or "" when the session is not inside a repository.
//
// Returning a non-repo directory here is a trap: callers use it as a
// base for `.worktrees/<slug>` operations, so a wrong answer sends those
// lookups somewhere unrelated instead of failing cleanly.
func sessionPrimary(s *session.Session) string {
	ws := sessionWorkspace(s)
	if ws == "" {
		return ""
	}
	return wtbridge.GitPrimary(ws)
}

// resolveRepoPath returns the directory to act on for this session: the
// worktree for a worktree-rooted session, the checkout for a primary one.
func (m *model) resolveRepoPath(s *session.Session) string {
	if s == nil {
		return ""
	}
	if s.WorktreeSlug != "" {
		if primary := sessionPrimary(s); primary != "" {
			if real := scanner.SlugFromEncoded(primary, s.WorktreeSlug); real != "" {
				p := filepath.Join(primary, ".worktrees", real)
				if st, err := os.Stat(p); err == nil && st.IsDir() {
					return p
				}
			}
			if p, err := m.wt.PathFor(primary, s.WorktreeSlug); err == nil && p != "" {
				return p
			}
		}
	}
	return sessionWorkspace(s)
}

// runWtCmd is a small helper that turns a wtbridge call into a
// confirm-bar message — both success and failure paths.
func (m *model) runWtCmd(label string, do func() error) tea.Cmd {
	return func() tea.Msg {
		if err := do(); err != nil {
			return wtResultMsg{title: label + " failed", subtitle: err.Error(), failed: true}
		}
		return wtResultMsg{title: label, subtitle: ""}
	}
}

// runRemoveCmd is runWtCmd for `r`: on failure it lifts the card's
// tombstone so the still-existing card comes back.
func (m *model) runRemoveCmd(label, key string, do func() error) tea.Cmd {
	return func() tea.Msg {
		if err := do(); err != nil {
			return wtResultMsg{title: label + " failed", subtitle: err.Error(), failed: true, restore: key}
		}
		return wtResultMsg{title: label, subtitle: ""}
	}
}

// OrphanedWorktree reports whether a card should be dropped because its
// worktree no longer exists.
//
// Removal deletes the worktree but never the transcript, and card
// tombstones are in-memory, so without this a removed card returns on the
// next restart — presenting itself as a primary session on the parent
// repo, which it never was.
//
// A live agent keeps its card even in a deleted worktree: that is exactly
// when you need the dashboard to reach in and kill it.
func OrphanedWorktree(s session.Session, live bool) bool {
	if live || s.WorktreeSlug == "" {
		return false
	}
	return scanner.IsOrphanedWorktree(s.ProjectDir)
}

// tombstoneKey identifies a card for `r` removal. Worktree cards are
// keyed by project dir (one card per worktree, and stub cards share a
// placeholder id); primary cards by chat id.
func tombstoneKey(s session.Session) string {
	if s.WorktreeSlug != "" {
		return "wt:" + s.ProjectDir
	}
	return s.ID
}

// pbcopy writes <s> to the macOS clipboard. Best effort.
func pbcopy(s string) error {
	cmd := exec.CommandContext(context.Background(), "pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// fmt.Sprintf wrapper kept for grepability of all wt action labels.
func actionLabel(verb, id string) string { return fmt.Sprintf("%s %s", verb, id) }
