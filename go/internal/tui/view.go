package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// styleTable holds every reusable lipgloss style. Initialized once;
// lipgloss caches rendering internally so style reuse is essentially
// free.
var (
	styleHeader   = lipgloss.NewStyle().Bold(true)
	styleHeaderHR = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleSel      = lipgloss.NewStyle().Reverse(true)

	styleStateActive   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))  // bright green
	styleStateThinking = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))  // bright yellow
	styleStateIdle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // grey
	stylePaused        = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))  // bright magenta

	styleCardBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleLabel      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleValue      = lipgloss.NewStyle()
	styleValueBold  = lipgloss.NewStyle().Bold(true)

	styleConfirm    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleConfirmErr = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))

	styleHelpTitle = lipgloss.NewStyle().Bold(true)
)

func styleForState(state session.State) lipgloss.Style {
	switch state {
	case session.StateActive:
		return styleStateActive
	case session.StateThinking:
		return styleStateThinking
	case session.StateIdle:
		return styleStateIdle
	case session.StatePaused:
		return stylePaused
	}
	return styleDim
}

// View is the bubbletea entry point for rendering.
func (m *model) View() string {
	if m.width == 0 {
		// Pre-WindowSizeMsg: emit a single space so bubbletea doesn't
		// blank the screen with no measurement available.
		return " "
	}
	switch m.state {
	case stateHelp:
		return m.viewHelp()
	case stateNewWt:
		return m.viewNewWt()
	}
	return m.viewList()
}

func (m *model) viewList() string {
	var b strings.Builder

	// Header.
	left := styleHeader.Render(" agent-monitor ")
	right := fmt.Sprintf(" %s  ·  %d sessions ", time.Now().Format("15:04:05"), len(m.sessions))
	if m.paused {
		right = " [PAUSED]  " + right
	}
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 0 {
		pad = 0
	}
	b.WriteString(left + strings.Repeat(" ", pad) + styleDim.Render(right))
	b.WriteString("\n")
	b.WriteString(styleHeaderHR.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	// Cards.
	if len(m.sessions) == 0 {
		b.WriteString("\n  ")
		b.WriteString(styleDim.Render("no active sessions (set AM_IDLE_THRESHOLD_MIN higher to surface older ones)"))
		b.WriteString("\n")
	} else {
		for i, s := range m.sessions {
			b.WriteString(m.renderCard(s, i == m.selected))
		}
	}

	// Confirm bar.
	if m.confirm.title != "" {
		b.WriteString("\n")
		stl := styleConfirm
		if strings.Contains(strings.ToLower(m.confirm.title), "fail") {
			stl = styleConfirmErr
		}
		b.WriteString("  " + stl.Render(m.confirm.title))
		if m.confirm.subtitle != "" {
			b.WriteString("\n  " + styleDim.Render(m.confirm.subtitle))
		}
		b.WriteString("\n")
	}

	// Footer (full-width). Drop sections gracefully on narrow terminals.
	footer := m.renderFooter()
	b.WriteString("\n")
	b.WriteString(footer)
	return b.String()
}

func (m *model) renderCard(s session.Session, selected bool) string {
	totalW := m.width - 2
	if totalW < 50 {
		totalW = 50
	}
	state := s.State
	stl := styleForState(state)

	selmark := "  "
	if selected {
		selmark = styleSel.Render("▶ ")
	}

	ago := agoString(s.Mtime)
	wt := s.WorktreeSlug
	if wt == "" {
		wt = "(primary)"
	}

	stateText := stl.Render(string(state))
	title := fmt.Sprintf(" %s  ·  %s ", styleValueBold.Render(s.Project), stateText)
	rightLabel := fmt.Sprintf(" %s ", styleDim.Render(ago))
	dashCount := totalW - lipgloss.Width(title) - lipgloss.Width(rightLabel) - 4
	if dashCount < 2 {
		dashCount = 2
	}
	dashes := styleCardBorder.Render(strings.Repeat("─", dashCount))
	top := selmark +
		styleCardBorder.Render("┌─") +
		title +
		dashes +
		rightLabel +
		styleCardBorder.Render("─┐")

	contentW := totalW - 4
	rows := []string{
		renderRow(contentW, "project:", fmt.Sprintf("%s   worktree: %s", s.Project, wt), styleValue),
		renderRow(contentW, "tool:", emptyOr(s.LastTool, "(thinking)"), styleValue),
	}
	if s.LastText != "" {
		quoted := `"` + s.LastText + `"`
		rows = append(rows, renderRow(contentW, "last:", quoted, styleValue))
	}
	rows = append(rows,
		renderRow(contentW, "id:", s.ID, styleValueBold),
		renderRow(contentW, "cmd:", "cursor-agent --resume "+s.ID, styleDim),
	)

	bot := "  " + styleCardBorder.Render("└"+strings.Repeat("─", totalW-2)+"┘")

	return top + "\n" + strings.Join(rows, "\n") + "\n" + bot + "\n"
}

func renderRow(contentW int, label, value string, valStyle lipgloss.Style) string {
	labelW := 9
	valW := contentW - labelW - 1
	if valW < 1 {
		valW = 1
	}
	if lipgloss.Width(value) > valW {
		// Truncate to fit, leaving room for ellipsis.
		value = truncateRunes(value, valW-1) + "…"
	}
	pad := valW - lipgloss.Width(value)
	if pad < 0 {
		pad = 0
	}
	return fmt.Sprintf("  %s %s %s%s %s",
		styleCardBorder.Render("│"),
		styleLabel.Render(padRight(label, labelW)),
		valStyle.Render(value), strings.Repeat(" ", pad),
		styleCardBorder.Render("│"),
	)
}

func padRight(s string, w int) string {
	pad := w - lipgloss.Width(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func emptyOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func agoString(mtime int64) string {
	d := time.Now().Unix() - mtime
	switch {
	case d < 60:
		return fmt.Sprintf("%ds", d)
	case d < 3600:
		return fmt.Sprintf("%dm", d/60)
	default:
		return fmt.Sprintf("%dh", d/3600)
	}
}

func (m *model) renderFooter() string {
	full := "[j/k] move  [f] follow  [a] attach  [n] new  [i] ide  [x] pause-agent  [r] remove  [?] help"
	mid := "[j/k] move  [f] follow  [a] attach  [n] new  [i] ide  [?] help"
	short := "[j/k] move  [f] follow  [a] attach  [n] new  [i] ide"
	rhs := fmt.Sprintf("%d sessions", len(m.sessions))

	candidates := []string{full, mid, short}
	for _, lhs := range candidates {
		pad := m.width - lipgloss.Width(lhs) - lipgloss.Width(rhs)
		if pad >= 1 {
			return styleDim.Render(lhs+strings.Repeat(" ", pad)+rhs) + "\n"
		}
	}
	return styleDim.Render(short) + "\n"
}

func (m *model) viewHelp() string {
	body := `agent-monitor — keyboard reference

Navigation
  j / ↓        next card
  k / ↑        previous card
  g / G        first / last card
  Enter        focus selected card in wt-manager pane

Multi-agent
  f            follow (live transcript tail in iTerm)
  a            resume in new iTerm tab
  d            back to wt-manager
  y            copy chat id to clipboard
  o            open worktree in Cursor
  i            open nvim + Diffview iTerm tab

Worktree lifecycle
  n            new worktree (modal: branch / base / prompt)
  r            kill agent (and remove worktree if attached)
  s            wt sync (rebase onto base)

Agent control
  x            pause / resume the selected agent (SIGSTOP / SIGCONT)

Dashboard control
  p            pause / resume auto-refresh
  q / D        detach tmux (dashboard keeps running)
  Q            kill the agent-monitor tmux session
  ?            this help

Press any key to close.`
	return styleHelpTitle.Render(body)
}

func (m *model) viewNewWt() string {
	var b strings.Builder
	b.WriteString(styleHelpTitle.Render("new worktree (Esc to cancel, Tab to cycle, Enter to confirm)"))
	b.WriteString("\n\n")
	b.WriteString("  branch: " + m.form.branch.View() + "\n")
	b.WriteString("  base:   " + m.form.base.View() + "\n")
	b.WriteString("  prompt: " + m.form.prompt.View() + "\n")
	b.WriteString("\n")
	b.WriteString(styleDim.Render("  base defaults to origin/main · prompt is sent to cursor-agent on launch"))
	return b.String()
}
