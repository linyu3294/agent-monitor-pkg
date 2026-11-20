// Claude Code liveness.
//
// Cursor chats bind through an open store.db and Codewhale sessions
// through an open live marker — both are file descriptors held for the
// life of a session. Claude Code has no such descriptor: it opens its
// transcript only to append a record and closes it again, so lsof sees a
// claude process with nothing session-shaped open.
//
// What a claude process does carry is its working directory, and Claude
// Code records that same directory in every transcript record. So the
// binding is:
//
//  1. `ps` finds live claude processes.
//  2. The bulk `lsof` (already running for the other agent families)
//     yields each one's `cwd` descriptor — and, when the CLI happens to
//     hold its own transcript open, that file too.
//  3. An open transcript names its session exactly, so it wins outright.
//     Otherwise the dashboard joins cwd → the session written at or after
//     that process started, via BindClaudeSessions.
//
// Both joins are deliberately conservative, because a directory can hold
// several transcripts and the transcript cannot say which one a pid
// owns. The workspace join refuses when two live claude processes share
// a directory or when more than one transcript was written since the
// process started (a live session alongside a finished batch run). An
// unbound card merely loses its ACTIVE state; a wrong binding would let
// pause/kill signal someone else's agent.
package proctable

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// claudeProc is one live Claude Code process and the directory it runs
// in. cwd is empty when lsof did not report one. exact is true when the
// process holds its own session transcript open, in which case the
// session is already bound in byChat and cwd is only corroboration.
type claudeProc struct {
	pid     int
	state   byte
	started time.Time
	cwd     string
	exact   bool
}

// claudeStartSlack absorbs the gap between a process's reported start
// time and the first record it writes to its transcript.
const claudeStartSlack = 90 * time.Second

// defaultClaudeDir resolves the Claude Code transcript store.
func defaultClaudeDir() string {
	if dir := os.Getenv("AM_CLAUDE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// looksLikeClaude matches the three shapes the Claude Code CLI is
// installed in:
//
//	/opt/homebrew/bin/claude              (homebrew / native symlink)
//	~/.local/bin/claude                   (native installer symlink)
//	~/.local/share/claude/versions/<v>    (native installer binary)
//	.../@anthropic-ai/claude-code/cli.js  (npm install, run by node)
//
// basename comparison, not substring: a process that merely mentions
// "claude" in its arguments (an editor, a grep, a shell command) is not
// the CLI.
func looksLikeClaude(cmd []byte) bool {
	first := argv0(cmd)
	if bytes.Equal(baseName(first), []byte("claude")) {
		return true
	}
	if bytes.Contains(first, []byte("/.local/share/claude/")) {
		return true
	}
	// npm install: the marker sits in argv[1], so the command line is
	// searched — but only when argv[0] is an interpreter that could be
	// running a script, otherwise `cat …/@anthropic-ai/…` matches.
	if bytes.Contains(cmd, []byte("/@anthropic-ai/claude-code/")) {
		switch string(baseName(first)) {
		case "node", "bun", "deno", "cli.js":
			return true
		}
	}
	return false
}

// markClaudeExact records that this pid's session is already known from
// its open transcript. Any cwd entry captured for the same pid in the
// same lsof pass is folded into it: a duplicate would make its own
// workspace look ambiguous and could bind a second, wrong session.
func (t *Table) markClaudeExact(pid int, state byte, started time.Time) {
	t.dropClaudeProc(pid)
	t.claudeProcs = append(t.claudeProcs, claudeProc{pid: pid, state: state, started: started, exact: true})
}

// setClaudeCwd records the directory a pid runs in, unless that pid is
// already bound exactly (its transcript named the session, which is
// better evidence than any workspace guess).
func (t *Table) setClaudeCwd(pid int, state byte, started time.Time, cwd string) {
	for i := range t.claudeProcs {
		if t.claudeProcs[i].pid != pid {
			continue
		}
		if t.claudeProcs[i].exact {
			return
		}
		t.claudeProcs[i].cwd = cwd
		t.claudeProcs[i].state = state
		t.claudeProcs[i].started = started
		return
	}
	t.claudeProcs = append(t.claudeProcs, claudeProc{pid: pid, state: state, started: started, cwd: cwd})
}

// dropClaudeProc removes every entry for pid, preserving order.
func (t *Table) dropClaudeProc(pid int) {
	kept := t.claudeProcs[:0]
	for _, p := range t.claudeProcs {
		if p.pid != pid {
			kept = append(kept, p)
		}
	}
	t.claudeProcs = kept
}

// ClaudeSessionRef is one Claude session that ran in a workspace, with
// the mtime of its transcript. ClaudeWorkspaceIndex builds these.
type ClaudeSessionRef struct {
	ID    string
	Mtime int64
}

// BindClaudeSessions binds live claude processes to Claude sessions.
// byWorkspace maps a workspace directory to every scanned session that
// ran there; ClaudeWorkspaceIndex builds it from a scan.
//
// A process whose exact transcript is already bound (see
// claudeIDFromOpenPath) is left alone: an open session file names the
// session itself, which beats any workspace inference.
//
// The workspace join is deliberately conservative. A directory can hold
// several transcripts — previous sessions, batch `claude -p` runs — and
// the transcript alone cannot say which one a pid owns. So a candidate
// must have been written at or after the process started (a session the
// process finished before it started is not its session), and if more
// than one candidate survives, nothing is bound. A card that offers
// pause/kill must be able to name the process it is about to signal; an
// unbound card merely loses its ACTIVE state, while a wrong binding
// would signal someone else's agent.
func (t *Table) BindClaudeSessions(byWorkspace map[string][]ClaudeSessionRef) {
	if len(byWorkspace) == 0 || len(t.claudeProcs) == 0 {
		return
	}

	// Two live claude processes in one directory cannot be told apart,
	// whatever their transcripts say, so neither is bound.
	perWorkspace := map[string]int{}
	for _, p := range t.claudeProcs {
		if p.exact {
			continue
		}
		for _, key := range workspaceKeys(p.cwd) {
			perWorkspace[key]++
		}
	}

	bound := map[string]bool{}
	for _, p := range t.claudeProcs {
		if p.exact {
			continue
		}
		keys := workspaceKeys(p.cwd)
		if len(keys) == 0 {
			continue
		}
		ambiguousPid := false
		for _, key := range keys {
			if perWorkspace[key] > 1 {
				ambiguousPid = true
			}
		}
		if ambiguousPid {
			continue
		}

		// Collect the distinct sessions plausible for this process. A
		// process with no start time cannot rule anything out, so only a
		// single-session directory binds for it.
		cutoff := p.started.Add(-claudeStartSlack).Unix()
		if p.started.IsZero() {
			cutoff = math.MinInt64
		}
		seen := map[string]bool{}
		var candidates []ClaudeSessionRef
		for _, key := range keys {
			for _, ref := range byWorkspace[key] {
				if seen[ref.ID] || ref.Mtime < cutoff {
					continue
				}
				seen[ref.ID] = true
				candidates = append(candidates, ref)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		id := candidates[0].ID
		if id == "" || bound[id] {
			continue
		}
		if _, taken := t.byChat[id]; taken {
			continue
		}
		t.byChat[id] = Process{PID: p.pid, State: p.state}
		bound[id] = true
	}
}

// claudeIDFromOpenPath extracts the session id from a Claude Code
// transcript path:
//
//	<store>/<project-key>/<uuid>.jsonl
//
// The transcript is opened to append a record and closed again, so a
// process usually does not hold it. When one does, the open file names
// the session exactly and beats the workspace inference.
//
// Anything deeper is rejected on purpose: subagent transcripts live at
// `<store>/<key>/<uuid>/subagents/agent-*.jsonl`, and binding a card to a
// subagent's file would let `r` kill the parent session while naming a
// turn nobody can see.
func claudeIDFromOpenPath(name, store string) string {
	prefix := strings.TrimSuffix(store, "/") + "/"
	if store == "" || !strings.HasPrefix(name, prefix) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(name, prefix), "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".jsonl") {
		return ""
	}
	id := strings.TrimSuffix(parts[1], ".jsonl")
	if !claudeIDRe.MatchString(id) {
		return ""
	}
	return id
}

// claudeIDRe matches the UUID session ids Claude Code uses as transcript
// filenames.
var claudeIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ClaudeWorkspaceIndex maps each workspace directory to every scanned
// Claude session that ran there. BindClaudeSessions picks among them
// using the process's start time; nothing here guesses.
func ClaudeWorkspaceIndex(sessions []session.Session) map[string][]ClaudeSessionRef {
	out := map[string][]ClaudeSessionRef{}
	seen := map[string]bool{}
	for _, s := range sessions {
		if s.Source != session.SourceClaude || s.ID == "" || s.CWDHint == "" {
			continue
		}
		for _, key := range workspaceKeys(s.CWDHint) {
			dedupe := key + "\x00" + s.ID
			if seen[dedupe] {
				continue
			}
			seen[dedupe] = true
			out[key] = append(out[key], ClaudeSessionRef{ID: s.ID, Mtime: s.Mtime})
		}
	}
	return out
}

// workspaceKeys returns the spellings of a directory a process cwd and a
// transcript cwd might use: the cleaned path, and its symlink-resolved
// form when it differs (macOS /var and /tmp are symlinks, and lsof may
// report either spelling depending on whether the path came from the
// process's cwd or from an opened file).
func workspaceKeys(p string) []string {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil
	}
	clean := filepath.Clean(p)
	keys := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		keys = append(keys, resolved)
	}
	return keys
}

// claudeID resolves an open file against the Claude store, trying every
// spelling of the store path for the same symlink reason as
// workspaceKeys.
func (t *Table) claudeID(name string) string {
	for _, store := range workspaceKeys(t.claudeDir) {
		if id := claudeIDFromOpenPath(name, store); id != "" {
			return id
		}
	}
	return ""
}
