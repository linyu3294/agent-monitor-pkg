// Package session models a Cursor agent transcript and the metadata derived
// from its on-disk location.
//
// A Session is uniquely identified by its chat id (UUID v4). Cursor stores
// transcripts at:
//
//	$HOME/.cursor/projects/<encoded>/agent-transcripts/<chatId>/<chatId>.jsonl
//
// where <encoded> is the workspace path with `/` replaced by `-` (and the
// leading `/` dropped). For worktree-rooted sessions the encoded form ends
// in `-worktrees-<encoded-slug>`.
//
// All path-decoding logic in this package derives the user's home prefix
// from `os.UserHomeDir` so the tool is portable to any machine — the
// previous bash implementation hardcoded `Users-rebet-Documents-` and was
// machine-specific.
package session

import (
	"os"
	"path/filepath"
	"strings"
)

// State is the visible classification surfaced on each card.
type State string

const (
	StateActive   State = "ACTIVE"
	StateThinking State = "THINKING"
	StateIdle     State = "IDLE"
	StatePaused   State = "PAUSED"
)

// Role mirrors the role field on a JSONL transcript entry.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleUnknown   Role = ""
)

// Session is the in-memory view of a single chat's transcript and its
// derived metadata. Filled by the scanner on each tick.
type Session struct {
	ID             string // UUID
	TranscriptPath string // absolute path to <chatId>.jsonl
	ProjectDir     string // basename of the encoded project dir
	Project        string // human label (decoded, prefix-stripped)
	WorktreeSlug   string // empty when not in a worktree
	Mtime          int64  // unix seconds of last transcript write
	LastRole       Role
	LastTool       string // tool_use name on the last assistant entry, if any
	LastText       string // text on the last assistant entry, truncated
	CWDHint        string // first `cd <path>` seen in the transcript, if any
	State          State  // populated by the classifier
	PID            int    // 0 if no live cursor-agent holds the chat
	Paused         bool   // true if the held PID is in stopped (T) state
}

// HomePrefix returns the encoded `<home>` portion that Cursor prepends to
// every project dir name (e.g. "Users-rebet-" on macOS). Determined from
// `os.UserHomeDir()` so the tool is portable.
func HomePrefix() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	// Cursor encodes paths by stripping the leading `/` and replacing `/`
	// with `-`. So "/Users/rebet" becomes "Users-rebet".
	enc := strings.TrimPrefix(home, "/")
	enc = strings.ReplaceAll(enc, "/", "-")
	return enc + "-"
}

// ProjectLabel produces a short, human-readable project name from the
// encoded project-dir basename. Strips the user's home prefix and a
// trailing `-Documents-` segment if present, since those add noise to
// every card.
func ProjectLabel(projectDir string) string {
	label := projectDir
	if pre := HomePrefix(); pre != "" && strings.HasPrefix(label, pre) {
		label = strings.TrimPrefix(label, pre)
	}
	// Many users keep their work under ~/Documents — strip that too so
	// the card shows the repo name rather than the path.
	label = strings.TrimPrefix(label, "Documents-")
	return label
}

// ProjectDirOf returns the encoded project-dir basename for a transcript path:
//
//	$HOME/.cursor/projects/<encoded>/agent-transcripts/<id>/<id>.jsonl
//	                       └─ this ─┘
func ProjectDirOf(transcriptPath string) string {
	dir := filepath.Dir(transcriptPath)        // .../<id>
	dir = filepath.Dir(dir)                    // .../agent-transcripts
	dir = filepath.Dir(dir)                    // .../<encoded>
	return filepath.Base(dir)
}
