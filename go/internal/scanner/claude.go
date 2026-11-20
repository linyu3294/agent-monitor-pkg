// Claude Code session discovery.
//
// Claude Code keeps one JSONL transcript per session at
//
//	$HOME/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl
//
// and appends one record per conversation event: the user's prompt, the
// assistant's message (possibly carrying tool_use blocks), each tool
// result, and bookkeeping records (`summary`, `system`,
// `file-history-snapshot`, …). Every record states the workspace in a
// top-level `cwd` field.
//
// The directory name is Claude's own encoding (`-Users-me-repo`), and it
// is never decoded here: paths containing `.` or `_` encode ambiguously,
// so the card's workspace comes from the record's `cwd` and is stored in
// the same encoded form Cursor uses — the form every downstream helper
// (DecodeProjectPath, SlugFromEncoded, worktree dedupe) already knows.
//
// Reading is bounded like every other store: a fixed head window for the
// workspace and a backwards-growing tail window for the last turn.
package scanner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// claudeHeadBytes is the workspace-metadata window. Every record states
// `cwd`, so the first line is normally enough; the window is generous
// because a session can open with a long pasted prompt.
const claudeHeadBytes = 64 << 10

// claudeSessionIDRe matches the UUID session ids Claude Code uses as
// transcript filenames. The store also holds subagent transcripts and
// index files whose names are not session ids, and a resume command is
// built from this value — so the shape is validated rather than trusted.
var claudeSessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// claudeRecord is the subset of one transcript line this package reads.
// `type` is the record kind ("user"/"assistant"/"summary"/"system"/…);
// `message.role` repeats it for conversation records. Both are read
// because the two have disagreed across releases.
type claudeRecord struct {
	Type        string `json:"type"`
	Cwd         string `json:"cwd"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudeBlock is one content block inside an assistant message.
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// claudeCandidates lists transcripts worth a card.
//
// Worktree-backed sessions are immortal, matching the Cursor and
// Codewhale contract of "one card per worktree, always": an idle agent
// in a worktree must stay reachable so it can be resumed or removed
// from the dashboard. The workspace needed to decide that is cached, so
// a quiescent store still does zero reads.
func (s *Scanner) claudeCandidates(cutoff int64) []candidate {
	root := s.opts.ClaudeDir
	if root == "" {
		return nil
	}
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []candidate
	touched := make(map[string]struct{})
	for _, pe := range projects {
		if !pe.IsDir() {
			continue
		}
		// Expected shape: <root>/<project-key>/<session-id>.jsonl.
		// One directory level is deliberate — subagent transcripts live
		// under a per-session directory and must not become cards.
		entries, err := os.ReadDir(filepath.Join(root, pe.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !claudeSessionIDRe.MatchString(id) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(root, pe.Name(), name)
			touched[path] = struct{}{}
			m := info.ModTime().Unix()
			if m < cutoff && !s.claudeOnLiveWorktree(path, m) {
				continue
			}
			out = append(out, candidate{
				path:   path,
				mtime:  m,
				id:     id,
				source: session.SourceClaude,
			})
		}
	}
	s.gcWhCwd(root, touched)
	return out
}

// claudeOnLiveWorktree reports whether an aged-out transcript belongs to
// a worktree that still exists on disk.
func (s *Scanner) claudeOnLiveWorktree(path string, mtime int64) bool {
	return s.agedOutWorktreeIsLive(path, mtime, parseClaudeCWD)
}

// claudeSession builds one card from a candidate transcript, reusing the
// mtime cache so a growing tail doesn't re-read the whole session.
func (s *Scanner) claudeSession(c candidate) session.Session {
	sess := session.Session{
		ID:             c.id,
		Source:         session.SourceClaude,
		TranscriptPath: c.path,
		Mtime:          c.mtime,
	}

	entry, hit := s.cache[c.path]
	cwd := entry.cwdHint
	if !hit || (cwd == "" && entry.mtime != c.mtime) {
		// Re-read the head when the cached workspace is missing: a
		// transcript can be observed a tick before its first record is
		// flushed. Gated on mtime so a permanently unlabelled log isn't
		// re-read on every quiescent tick.
		cwd = parseClaudeCWD(c.path)
	}
	sess.CWDHint = cwd

	if hit && entry.mtime == c.mtime {
		sess.LastRole = entry.role
		sess.LastTool = entry.tool
		sess.LastText = entry.text
	} else {
		role, tool, text := parseClaudeTail(c.path)
		sess.LastRole = role
		sess.LastTool = tool
		sess.LastText = text
		s.cache[c.path] = cacheEntry{
			mtime:   c.mtime,
			role:    role,
			tool:    tool,
			text:    text,
			cwdHint: cwd,
		}
	}

	if enc := encodeCodewhaleCWD(cwd); enc != "" {
		sess.ProjectDir = enc
		sess.Project = session.ProjectLabel(enc)
		sess.WorktreeSlug = worktreeSlugFor(enc)
	} else {
		sess.Project = "(unknown workspace)"
	}
	return sess
}

// parseClaudeCWD reads the head of a transcript and returns the
// workspace its records state.
func parseClaudeCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	return claudeWorkspace(readWindow(f, 0, info.Size(), claudeHeadBytes))
}

// claudeWorkspace returns the first absolute workspace a record states.
// The value is checked with usableWorkspace, so a tool payload's path
// cannot be mistaken for the launch directory — except under
// `.worktrees/`, where a deleted directory must still be recognised as a
// worktree (see usableWorkspace).
func claudeWorkspace(buf []byte) string {
	scn := bufio.NewScanner(bytes.NewReader(buf))
	scn.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scn.Scan() {
		line := bytes.TrimSpace(scn.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec claudeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if v := strings.TrimSpace(rec.Cwd); usableWorkspace(v) {
			return v
		}
	}
	return ""
}

// parseClaudeTail reads the transcript's last turn from a bounded tail,
// growing the window backwards while every record in it is oversized.
func parseClaudeTail(path string) (session.Role, string, string) {
	f, err := os.Open(path)
	if err != nil {
		return session.RoleUnknown, "", ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return session.RoleUnknown, "", ""
	}
	role, tool, text, _ := lastTurnBackward(f, info.Size(), claudeLastTurn)
	return role, tool, text
}

// claudeLastTurn walks the tail backwards and returns the role, tool
// name, and text of the newest conversation record.
//
// Two record kinds are deliberately skipped:
//
//   - `isSidechain` records, which belong to a subagent. A subagent
//     finishing after the main thread would otherwise paint the card
//     with a turn the user never sees.
//   - records that are not conversation turns (summary, system,
//     file-history-snapshot, …). They carry no role, and the previous
//     turn is the honest answer.
func claudeLastTurn(buf []byte) (session.Role, string, string) {
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec claudeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.IsSidechain {
			continue
		}
		role := claudeRole(rec)
		if role == session.RoleUnknown {
			continue
		}
		tool, text := claudeContent(role, rec.Message.Content)
		text = sanitize(text)
		if len(text) > 200 {
			text = truncRunes(text, 200)
		}
		return role, tool, text
	}
	return session.RoleUnknown, "", ""
}

// claudeRole prefers the message's role and falls back to the record
// type. A tool-result record is a `user` turn — the model is about to
// generate — which is exactly what the classifier wants to see.
func claudeRole(rec claudeRecord) session.Role {
	switch strings.ToLower(rec.Message.Role) {
	case "assistant":
		return session.RoleAssistant
	case "user":
		return session.RoleUser
	}
	switch strings.ToLower(rec.Type) {
	case "assistant":
		return session.RoleAssistant
	case "user":
		return session.RoleUser
	}
	return session.RoleUnknown
}

// claudeContent interprets an assistant message's content, which is
// normally a list of typed blocks. Text and tool are taken only from
// assistant turns: a user prompt is not the agent's last words, and the
// card's `last:` row would be misleading if it showed one.
func claudeContent(role session.Role, raw json.RawMessage) (tool, text string) {
	if role != session.RoleAssistant || len(raw) == 0 {
		return "", ""
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if b.Name != "" {
					tool = b.Name
				}
			case "text":
				if b.Text != "" {
					text = b.Text
				}
			}
		}
		return tool, text
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return "", plain
	}
	return "", ""
}
