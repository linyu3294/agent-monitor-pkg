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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textinput"

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
	branch  textinput.Model
	base    textinput.Model
	prompt  textinput.Model
	focused int // 0=branch, 1=base, 2=prompt
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

	state    uiState
	confirm  confirmBar
	form     newWtForm
	tombstones map[string]struct{} // chat ids removed via `r` on a primary card

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
}

// confirmExpireMsg clears the bottom bar.
type confirmExpireMsg time.Time

// wtResultMsg surfaces the outcome of a `wt`/process action.
type wtResultMsg struct {
	title    string
	subtitle string
	failed   bool
}

// New builds an initial model.
func New(opts Options, version string) *model {
	scn := scanner.New(scanner.Options{
		IdleThreshold: opts.IdleThreshold,
		MaxCards:      opts.MaxCards,
	})
	branch := textinput.New()
	branch.Prompt = ""
	branch.CharLimit = 80
	branch.Width = 50
	base := textinput.New()
	base.Prompt = ""
	base.CharLimit = 80
	base.Width = 50
	base.SetValue("origin/main")
	prompt := textinput.New()
	prompt.Prompt = ""
	prompt.CharLimit = 200
	prompt.Width = 50
	return &model{
		opts:    opts,
		scanner: scn,
		proc:    proctable.New(),
		wt:      wtbridge.New(),
		state:   stateList,
		form: newWtForm{
			branch: branch,
			base:   base,
			prompt: prompt,
		},
		tombstones: map[string]struct{}{},
		version:    version,
	}
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
//     first one wins).
//   - Primary-checkout sessions surface as individual cards — the user can
//     run multiple agents against the main checkout and remove them
//     independently with `r`.
func (m *model) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		m.proc.Refresh()
		raw := m.scanner.Scan()
		var wtmID string
		filtered := raw[:0]
		seenWorktree := map[string]bool{}
		for _, s := range raw {
			if _, ok := m.tombstones[s.ID]; ok {
				continue
			}
			if m.proc.IsWtManager(s.ID) {
				wtmID = s.ID
				continue
			}
			if s.WorktreeSlug != "" {
				if seenWorktree[s.ProjectDir] {
					continue
				}
				seenWorktree[s.ProjectDir] = true
			}
			s.State = classifier.Classify(s, m.proc)
			s.PID = m.proc.PID(s.ID)
			s.Paused = m.proc.IsPaused(s.ID)
			filtered = append(filtered, s)
		}
		return scanResultMsg{sessions: filtered, wtManagerID: wtmID}
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

// resolveRepoPath returns the on-disk workspace root for the given
// session (worktree path for worktree-rooted sessions, primary repo
// otherwise). Returns "" if it can't be resolved.
func (m *model) resolveRepoPath(s *session.Session) string {
	primary := scanner.DecodeProjectPath(s.ProjectDir)
	if primary == "" {
		return ""
	}
	if s.WorktreeSlug == "" {
		return primary
	}
	// Translate the encoded slug into an actual on-disk slug.
	real := scanner.SlugFromEncoded(primary, s.WorktreeSlug)
	if real != "" {
		p := filepath.Join(primary, ".worktrees", real)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := m.wt.PathFor(primary, s.WorktreeSlug); err == nil && p != "" {
		return p
	}
	return primary
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

// pbcopy writes <s> to the macOS clipboard. Best effort.
func pbcopy(s string) error {
	cmd := exec.CommandContext(context.Background(), "pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// fmt.Sprintf wrapper kept for grepability of all wt action labels.
func actionLabel(verb, id string) string { return fmt.Sprintf("%s %s", verb, id) }
