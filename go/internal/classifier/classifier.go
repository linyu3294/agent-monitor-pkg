// Package classifier turns a session's transcript shape + process state
// into the visible card label.
//
// This replaces the old mtime-window-only classifier in `sessions.sh`
// which mislabeled finished turns as THINKING for ~2 minutes after every
// reply. The fix is to read the *content* of the last JSONL entry:
//
//	role=assistant + tool_use → ACTIVE  (tool in flight)
//	role=assistant + text only → IDLE   (turn ended, awaiting user)
//	role=user                  → THINKING (model is generating)
//
// mtime is still used as a coarse fallback for >2 min staleness and as
// the source of "last seen" — but it never overrides content shape.
package classifier

import (
	"time"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// View is the subset of process-table state the classifier needs.
type View interface {
	IsLive(chatID string) bool
	IsPaused(chatID string) bool
}

// trustsLiveProcess reports whether a live process is stronger evidence
// than the transcript clock.
//
// For Codewhale it is, and the reason is mechanical rather than stylistic:
// a session counts as live only when a codewhale process holds its
// live-marker file descriptor open, which a dead session cannot do. The
// log, meanwhile, is rewritten per turn, so its mtime lags real work by
// however long the current turn's tools take. Deferring to the clock
// there produces the worst kind of wrong answer — an agent running a
// long command shows IDLE.
//
// Cursor keeps the clock rule: its pids can idle without recording
// anything, so a stale transcript there means an idle chat.
func trustsLiveProcess(s session.Session) bool {
	return s.Source == session.SourceCodewhale
}

// Classify returns the visible state for a session given the current
// process snapshot.
func Classify(s session.Session, p View) session.State {
	if p.IsPaused(s.ID) {
		return session.StatePaused
	}
	if !p.IsLive(s.ID) {
		// Dead process trumps everything: can't be ACTIVE/THINKING if
		// nothing is running.
		return session.StateIdle
	}
	now := time.Now().Unix()
	delta := now - s.Mtime
	if delta > 120 && !trustsLiveProcess(s) {
		// Stale beyond the threshold — IDLE regardless of role.
		//
		// This rule exists because a Cursor pid can outlive its work
		// without touching its transcript, so the clock is the honest
		// signal there. It does not hold for Codewhale, whose log is
		// written per turn: a long tool call leaves the file untouched
		// for minutes while the agent is unmistakably working. See
		// trustsLiveProcess.
		return session.StateIdle
	}
	// Within a 5-second grace window the file may be mid-flush, so we
	// optimistically call it ACTIVE. This avoids a sub-second blip
	// between mtime ticks where a fresh write parses to a partial
	// object.
	if delta <= 5 {
		return session.StateActive
	}

	switch s.LastRole {
	case session.RoleAssistant:
		if s.LastTool != "" {
			return session.StateActive
		}
		return session.StateIdle
	case session.RoleUser:
		return session.StateThinking
	}
	// Unknown role — fall back to a conservative time window so we
	// don't regress on legacy transcripts.
	if delta <= 30 {
		return session.StateActive
	}
	if s.LastTool != "" {
		return session.StateActive
	}
	return session.StateIdle
}
