package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// Dracula-inspired accent palette. Centralized so re-theming is one
// edit away. Hex literals degrade gracefully on 256-color terminals
// via lipgloss's color profile detection (same mechanism that already
// lets `colorPink` work as the modal border).
var (
	colorPink       = lipgloss.Color("#FF10F0") // neon pink — modal frame, header title, ▶ caret
	colorPinkSoft   = lipgloss.Color("#FF79C6") // softer pink — paused state
	colorPurple     = lipgloss.Color("#BD93F9") // bright purple — accent borders, thinking state, footer
	colorPurpleDim  = lipgloss.Color("#6272A4") // dim purple — card content border, secondary chrome
	colorGreen      = lipgloss.Color("#7FE3C4") // mint — active state, chat ids (matches tmux status bar)
	colorYellow     = lipgloss.Color("#F1FA8C") // sand yellow — confirm bar (warning, not danger)
	colorRed        = lipgloss.Color("#FF5555") // soft red — confirm bar errors
	colorGrey       = lipgloss.Color("245")     // unchanged neutral grey for idle
	colorGreyDim    = lipgloss.Color("8")       // unchanged terminal-default dim
)

// styleTable holds every reusable lipgloss style. Initialized once;
// lipgloss caches rendering internally so style reuse is essentially
// free.
var (
	styleHeader   = lipgloss.NewStyle().Bold(true).Foreground(colorPink)
	styleHeaderHR = lipgloss.NewStyle().Foreground(colorPurple)
	styleDim      = lipgloss.NewStyle().Foreground(colorGreyDim)
	styleSel      = lipgloss.NewStyle().Foreground(colorPink).Bold(true)

	styleStateActive   = lipgloss.NewStyle().Foreground(colorGreen).Bold(true) // neon green
	styleStateThinking = lipgloss.NewStyle().Foreground(colorPurple).Bold(true) // purple (was yellow)
	styleStateIdle     = lipgloss.NewStyle().Foreground(colorGrey)              // grey, unchanged
	stylePaused        = lipgloss.NewStyle().Foreground(colorPinkSoft).Bold(true) // soft pink

	// Card borders: title row gets the brighter accent so each card's
	// header pops; the content rows use a dimmer purple so the values
	// stay the visual focus.
	styleCardBorderTop = lipgloss.NewStyle().Foreground(colorPurple)
	styleCardBorder    = lipgloss.NewStyle().Foreground(colorPurpleDim)
	styleLabel         = lipgloss.NewStyle().Foreground(colorGreyDim)
	styleValue         = lipgloss.NewStyle()
	styleValueBold     = lipgloss.NewStyle().Bold(true)
	styleValueId       = lipgloss.NewStyle().Foreground(colorGreen).Bold(true) // chat-id row pops

	styleConfirm    = lipgloss.NewStyle().Foreground(colorYellow).Bold(true)
	styleConfirmErr = lipgloss.NewStyle().Foreground(colorRed).Bold(true)

	styleFooter    = lipgloss.NewStyle().Foreground(colorPurple)
	styleHelpTitle = lipgloss.NewStyle().Bold(true).Foreground(colorPink)
	// styleHelpBody is the modal body text. Bold but uncolored so the
	// long help text stays readable; the colored bits (the modal frame
	// and any embedded titles) provide all the accent we need.
	styleHelpBody = lipgloss.NewStyle().Bold(true)

	// Neon-pink rounded border used to frame full-screen modals (help,
	// new-worktree). Same hex as the header title for visual consistency.
	styleModalBorder = colorPink
	styleModal = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(styleModalBorder).
			Padding(1, 2)
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

	// Header. 2-col left + 2-col right margin so the title and timestamp
	// line up with the card content (cards render with a 2-col selmark
	// margin) and the right-aligned chrome doesn't kiss the terminal
	// edge. The horizontal rule below stays full-width on purpose so
	// the dashboard reads as one continuous frame.
	const headerLeftPad = "  "
	const headerRightPad = "  "
	left := styleHeader.Render("agent-monitor")
	right := fmt.Sprintf("%s  ·  %d sessions", time.Now().Format("15:04:05"), len(m.sessions))
	if m.hiddenEmpty > 0 {
		right += fmt.Sprintf("  ·  %d empty worktrees hidden [e]", m.hiddenEmpty)
	}
	if m.paused {
		right = "[PAUSED]  " + right
	}
	avail := m.width - lipgloss.Width(headerLeftPad) - lipgloss.Width(headerRightPad)
	gap := avail - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	b.WriteString(headerLeftPad + left + strings.Repeat(" ", gap) + styleDim.Render(right) + headerRightPad)
	b.WriteString("\n")
	b.WriteString(styleHeaderHR.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	// Cards (viewport-scrolled).
	//
	// Each card occupies a deterministic number of terminal rows
	// (cardHeight) — 6 normally, 7 when the card has a `last:` row
	// for the agent's last text reply. We compute the per-card heights
	// once, decide which slice fits in the available vertical space,
	// and adjust m.scrollOffset so the selected card is always visible.
	if len(m.sessions) == 0 {
		b.WriteString("\n  ")
		b.WriteString(styleDim.Render("no active sessions (set AM_IDLE_THRESHOLD_MIN higher to surface older ones)"))
		b.WriteString("\n")
	} else {
		heights := make([]int, len(m.sessions))
		for i, s := range m.sessions {
			heights[i] = cardHeight(s)
		}

		// Chrome rows reserved outside the card area: header (1) +
		// header rule (1) + blank-before-footer (1) + footer (1) = 4.
		// The confirm bar adds 1-2 more lines when active.
		chromeH := 4
		if m.confirm.title != "" {
			chromeH++
			if m.confirm.subtitle != "" {
				chromeH++
			}
			chromeH++ // trailing blank line below the bar
		}
		// Reserve 2 lines for the "▲ N more above" / "▼ N more below"
		// indicators so they never push a card off the bottom.
		viewport := m.height - chromeH - 2
		if viewport < cardHeightMin {
			viewport = cardHeightMin
		}

		// First, ensure scrollOffset doesn't sit above the selection.
		if m.scrollOffset > m.selected {
			m.scrollOffset = m.selected
		}
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
		// Walk forward from scrollOffset and find the last visible
		// index that still fits in the viewport. If the selected card
		// is past that point, advance scrollOffset one card at a time
		// until it comes into view.
		lastVis := lastVisibleIdx(heights, m.scrollOffset, viewport)
		for lastVis < m.selected && m.scrollOffset < m.selected {
			m.scrollOffset++
			lastVis = lastVisibleIdx(heights, m.scrollOffset, viewport)
		}
		if lastVis < m.scrollOffset {
			// Viewport is too small for even one card — show at least
			// the selected one (will overflow but gracefully).
			lastVis = m.scrollOffset
		}

		hiddenAbove := m.scrollOffset
		hiddenBelow := len(m.sessions) - 1 - lastVis

		if hiddenAbove > 0 {
			b.WriteString(styleDim.Render(fmt.Sprintf("  ▲ %d more above\n", hiddenAbove)))
		} else {
			b.WriteString("\n")
		}
		for i := m.scrollOffset; i <= lastVis && i < len(m.sessions); i++ {
			b.WriteString(m.renderCard(m.sessions[i], i == m.selected))
		}
		if hiddenBelow > 0 {
			b.WriteString(styleDim.Render(fmt.Sprintf("  ▼ %d more below\n", hiddenBelow)))
		} else {
			b.WriteString("\n")
		}
	}

	// Confirm bar.
	if m.confirm.title != "" {
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
	b.WriteString(footer)
	return b.String()
}

// cardHeightMin is the minimum height of any card (the no-`last:` form).
const cardHeightMin = 6

// cardHeight returns the number of terminal rows renderCard will emit
// for this session. Must stay in sync with renderCard's row count:
// 1 (top border) + N content rows + 1 (bottom border) where N is 4
// (project, tool, id, cmd) or 5 (with `last:`). The trailing newline
// in renderCard moves to the next row but doesn't add a blank line.
func cardHeight(s session.Session) int {
	if s.LastText != "" {
		return 7
	}
	return 6
}

// lastVisibleIdx returns the largest index i >= start such that the sum
// of heights[start..=i] fits within `cap`. Returns start-1 when no card
// fits (caller treats this as "viewport too small, show selected anyway").
func lastVisibleIdx(heights []int, start, cap int) int {
	cum := 0
	last := start - 1
	for i := start; i < len(heights); i++ {
		if cum+heights[i] > cap {
			break
		}
		cum += heights[i]
		last = i
	}
	return last
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
	// Card title row uses the brighter purple accent so each card's
	// header pops; the content rows below stay on the dimmer border
	// style so the values remain the visual focus.
	dashes := styleCardBorderTop.Render(strings.Repeat("─", dashCount))
	top := selmark +
		styleCardBorderTop.Render("┌─") +
		title +
		dashes +
		rightLabel +
		styleCardBorderTop.Render("─┐")

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
		renderRow(contentW, "id:", s.ID, styleValueId),
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

	// Match the card content's 2-col left margin (cards render with
	// `"  "` selmark or `"▶ "`) and a symmetric 2-col right margin so
	// the right-aligned session count doesn't kiss the terminal edge.
	const leftPad = "  "
	const rightPad = "  "
	avail := m.width - lipgloss.Width(leftPad) - lipgloss.Width(rightPad)

	candidates := []string{full, mid, short}
	for _, lhs := range candidates {
		gap := avail - lipgloss.Width(lhs) - lipgloss.Width(rhs)
		if gap >= 1 {
			return leftPad + styleFooter.Render(lhs+strings.Repeat(" ", gap)+rhs) + rightPad + "\n"
		}
	}
	return leftPad + styleFooter.Render(short) + "\n"
}

func (m *model) viewHelp() string {
	body := `agent-monitor — keyboard reference

Navigation
  j/↓  k/↑        next / previous card
  g / G            first / last card
  Enter            focus card in wt-manager

Multi-agent
  f    follow (live transcript tail)
  a    resume in new iTerm tab
  y    copy chat id to clipboard
  o    open in Cursor
  i    open nvim + Diffview (worktree only)

Worktree lifecycle
  n    new worktree (branch / base / prompt)
  r    remove selected (y/N confirm)
  s    wt sync (rebase onto base)

Agent control
  x    pause / resume (SIGSTOP / SIGCONT)

Dashboard
  p    pause / resume auto-refresh
  e    show / hide worktrees with no chat
  q/D  detach tmux
  Q    kill session
  ?    this help

Pane switching (tmux, session-scoped)
  C-Up / C-Down     top / bottom (recommended)
  F2 / F3           top / bottom (macOS: Fn+F2)
  M-1 / M-2         top / bottom (Option=Esc+)
  prefix Tab/BTab   cycle panes (always works)

Press any key to close.`
	return m.centerInModal(styleHelpBody.Render(body))
}

func (m *model) viewNewWt() string {
	var b strings.Builder
	b.WriteString(styleHelpTitle.Render("new worktree"))
	b.WriteString("\n")
	b.WriteString(styleDim.Render("Esc to cancel · Tab to cycle · Enter to confirm"))
	b.WriteString("\n\n")
	b.WriteString("branch: " + m.form.branch.View() + "\n")
	b.WriteString("base:   " + m.form.base.View() + "\n")
	b.WriteString("prompt: " + m.form.prompt.View() + "\n")
	b.WriteString("\n")
	b.WriteString(styleDim.Render("base defaults to origin/main · prompt is sent to cursor-agent on launch"))
	return m.centerInModal(b.String())
}

// centerInModal wraps content in the neon-pink modal frame and centers
// the whole thing on the available terminal viewport. Falls back to a
// left-aligned modal if we don't have a height yet (pre-WindowSizeMsg
// edge case).
func (m *model) centerInModal(content string) string {
	framed := styleModal.Render(content)
	if m.height <= 0 || m.width <= 0 {
		return framed
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, framed)
}
