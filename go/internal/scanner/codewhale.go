// Codewhale session discovery.
//
// Codewhale keeps one append-only log per saved session at
//
//	$HOME/.codewhale/sessions/<uuid>.json
//
// (the extension is `.json`, the content is JSONL — `codewhale
// session-diagnostics` takes the same file and calls it a "saved session
// log"). The file grows for the life of the session, so readers here are
// bounded exactly like the Cursor one: a fixed head window for the
// workspace, a fixed tail window for the last turn.
//
// The log's record schema is Codewhale's business, not ours. It changes
// between releases, and pinning field names is how a card silently
// degrades to "(unknown workspace)". So the workspace is found in three
// progressively weaker but schema-free ways:
//
//  1. a known key holding an absolute path (`cwd`, `workspace`, …);
//  2. the same keys in a single-document (non-JSONL) log;
//  3. any quoted absolute path in the head window that exists on disk,
//     preferring one that is a git checkout.
//
// Step 3 is what makes this resilient: it needs no key at all, only that
// the log mentions the directory it runs in. Its hits are verified
// against the filesystem before they are believed, because a heuristic
// that guesses wrong produces a confidently mislabelled card.
package scanner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// codewhaleHeadBytes is the workspace-metadata window. Larger than the
// Cursor head window because one Codewhale record can carry a long
// system prompt before the workspace path appears.
const codewhaleHeadBytes = 512 << 10

// codewhaleTailFloor is the minimum tail window for last-turn parsing.
// A single turn record with tool output routinely exceeds the Cursor
// 8 KB default, and a window that lands inside one record shows nothing.
const codewhaleTailFloor = 64 << 10

// codewhaleTailCeiling bounds the backward scan. A turn record holding a
// large tool output can be several megabytes; past this point the card
// reports an unknown turn rather than reading an unbounded file.
const codewhaleTailCeiling = 8 << 20

// lastTurnBackward scans backwards in growing windows until a record
// with a usable role parses, and reports how many bytes it read. The
// first window can land entirely inside one oversized record, which
// yields no complete line at all, so a single fixed tail is not enough.
func lastTurnBackward(f *os.File, size int64) (session.Role, string, string, int64) {
	win := int64(codewhaleTailFloor)
	for {
		if win > size {
			win = size
		}
		start := size - win
		buf := readWindow(f, start, size, win)
		role, tool, text := codewhaleLastTurn(buf)
		if role != session.RoleUnknown {
			return role, tool, text, win
		}
		if win >= size || win >= codewhaleTailCeiling {
			return session.RoleUnknown, "", "", win
		}
		win *= 2
	}
}

// CodewhaleSessionID is reserved for a Codewhale card whose store entry
// has no filename-derived id. Session ids come from the log filename, so
// this is a guard for malformed stores rather than a normal value.
const CodewhaleSessionID = "(codewhale)"

// codewhaleIDRe matches the UUID session ids Codewhale uses as log
// filenames. The store also holds lock files, temp files and diagnostics
// snapshots that must not become cards.
var codewhaleIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// codewhalePathRe finds quoted absolute paths inside a record. Used only
// as the last-resort workspace extraction, and every hit is stat'ed.
var codewhalePathRe = regexp.MustCompile(`"(/[^"\\]{1,512})"`)

// isCodewhaleSessionID accepts a UUID, or an id that owns a directory in
// the store — the two signals every real session carries.
func isCodewhaleSessionID(storeDir, id string) bool {
	if codewhaleIDRe.MatchString(id) {
		return true
	}
	st, err := os.Stat(filepath.Join(storeDir, id))
	return err == nil && st.IsDir()
}

// codewhaleSession builds one card from a candidate log, reusing the
// mtime cache so a growing tail doesn't re-read the whole session.
func (s *Scanner) codewhaleSession(c candidate) session.Session {
	id := c.id
	if id == "" {
		id = CodewhaleSessionID
	}
	sess := session.Session{
		ID:             id,
		Source:         session.SourceCodewhale,
		TranscriptPath: c.path,
		Mtime:          c.mtime,
	}

	entry, hit := s.cache[c.path]
	cwd := entry.cwdHint
	if !hit || (cwd == "" && entry.mtime != c.mtime) {
		// Re-read the head when the cached workspace is missing: a log
		// can be observed a tick before its metadata record is written.
		// Gated on mtime so a permanently unlabelled log isn't re-read
		// on every quiescent tick.
		cwd = parseCodewhaleCWD(c.path)
	}
	sess.CWDHint = cwd

	if hit && entry.mtime == c.mtime {
		sess.LastRole = entry.role
		sess.LastTool = entry.tool
		sess.LastText = entry.text
	} else {
		role, tool, text := parseCodewhaleTail(c.path, s.codewhaleTailBytes())
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

	sess.ProjectDir = encodeCodewhaleCWD(sess.CWDHint)
	if sess.ProjectDir != "" {
		sess.Project = session.ProjectLabel(sess.ProjectDir)
		sess.WorktreeSlug = worktreeSlugFor(sess.ProjectDir)
	} else {
		sess.Project = "(unknown workspace)"
	}
	return sess
}

func (s *Scanner) codewhaleTailBytes() int64 {
	if s.opts.TailBytes > codewhaleTailFloor {
		return s.opts.TailBytes
	}
	return codewhaleTailFloor
}

// codewhaleCandidates lists session logs worth a card.
//
// Worktree-backed sessions are immortal, matching the Cursor contract of
// "one card per worktree, always": an idle agent in a worktree must stay
// reachable so it can be resumed or removed from the dashboard. The
// workspace needed to decide that is cached, so a quiescent store still
// does zero reads.
func (s *Scanner) codewhaleCandidates(cutoff int64) []candidate {
	root := s.opts.CodewhaleDir
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]candidate, 0, len(entries))
	touched := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if id == "" || strings.HasPrefix(id, ".") || strings.Contains(id, ".") {
			continue
		}
		if !isCodewhaleSessionID(root, id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(root, name)
		touched[path] = struct{}{}
		m := info.ModTime().Unix()
		if m < cutoff && !s.codewhaleOnLiveWorktree(path, m) {
			continue
		}
		out = append(out, candidate{
			path:   path,
			mtime:  m,
			id:     id,
			source: session.SourceCodewhale,
		})
	}
	// Drop workspace entries for logs that no longer exist, so a
	// long-lived dashboard doesn't hold a growing map of dead paths.
	for p := range s.whCwd {
		if _, ok := touched[p]; !ok {
			delete(s.whCwd, p)
		}
	}
	return out
}

// codewhaleOnLiveWorktree reports whether an aged-out session log
// belongs to a worktree that still exists on disk. The workspace is
// cached per (path, mtime) so the head read happens once, not on every
// tick.
func (s *Scanner) codewhaleOnLiveWorktree(path string, mtime int64) bool {
	entry, ok := s.whCwd[path]
	if !ok || entry.mtime != mtime {
		entry = whCwdEntry{mtime: mtime, cwd: parseCodewhaleCWD(path)}
		s.whCwd[path] = entry
	}
	enc := encodeCodewhaleCWD(entry.cwd)
	if enc == "" || worktreeSlugFor(enc) == "" {
		return false
	}
	return !isOrphanedWorktree(enc)
}

// docShape describes a single-document session log: how many messages it
// holds and the key structure of the newest few. Key names and short
// type/role values only — never message text.
func docShape(buf []byte) []string {
	var doc map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf), &doc); err != nil {
		return nil
	}
	top := make([]string, 0, len(doc))
	for k := range doc {
		top = append(top, k)
	}
	sort.Strings(top)
	out := []string{"top-level: " + strings.Join(top, ", ")}

	msgs, ok := doc["messages"].([]any)
	if !ok {
		return out
	}
	out = append(out, fmt.Sprintf("messages: %d", len(msgs)))
	if meta, ok := doc["metadata"].(map[string]any); ok && len(meta) > 0 {
		mk := make([]string, 0, len(meta))
		for k := range meta {
			mk = append(mk, k)
		}
		sort.Strings(mk)
		out = append(out, "metadata keys: "+strings.Join(mk, ", "))
	}
	n := 0
	for i := len(msgs) - 1; i >= 0 && n < 2; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		n++
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		shape := fmt.Sprintf("  [%d] {%s}", i, strings.Join(keys, ","))
		for _, k := range []string{"type", "role"} {
			if v, ok := m[k].(string); ok && v != "" && len(v) <= 24 {
				shape += " " + k + "=" + v
			}
		}
		out = append(out, shape)
		out = append(out, "    content: "+contentShape(m["content"]))
	}
	return out
}

// contentShape describes a message's content without revealing it: the
// Go type, and for block lists the block types and key names.
func contentShape(v any) string {
	switch c := v.(type) {
	case nil:
		return "absent"
	case string:
		return fmt.Sprintf("string (%d bytes)", len(c))
	case []any:
		var kinds []string
		seen := map[string]bool{}
		for _, item := range c {
			b, ok := item.(map[string]any)
			if !ok {
				kinds = append(kinds, fmt.Sprintf("%T", item))
				continue
			}
			t, _ := b["type"].(string)
			if t == "" {
				t = "(no type)"
			}
			if seen[t] {
				continue
			}
			seen[t] = true
			keys := make([]string, 0, len(b))
			for k := range b {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			kinds = append(kinds, t+"{"+strings.Join(keys, ",")+"}")
		}
		return fmt.Sprintf("[]any len=%d [%s]", len(c), strings.Join(kinds, " "))
	case map[string]any:
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "object{" + strings.Join(keys, ",") + "}"
	}
	return fmt.Sprintf("%T", v)
}

// codewhaleDoc is Codewhale's session-log document. The store is one
// JSON object per session — not JSONL — shaped like:
//
//	{"schema_version":1, "metadata":{...,"workspace":"/abs/path"},
//	 "messages":[{"role":"assistant","content":[{"type":"text","text":"..."},
//	                                  {"type":"tool_use","name":"bash"}]}],
//	 "journal":..., "work_state":..., "approval_receipts":...}
//
// Reading this line-by-line (the Cursor shape) silently returns the
// FIRST message rather than the last turn, which is why an actively
// working session showed as IDLE.
type codewhaleDoc struct {
	Messages []struct {
		Role string `json:"role"`
		// Content is either a bare string (simple prompts and replies)
		// or a list of typed blocks. Decoding it as a slice rejects the
		// string form and fails the whole document, so it stays raw
		// here and is interpreted per message.
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Metadata map[string]any `json:"metadata"`
}

// codewhaleBlock is one content block inside a message.
type codewhaleBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	Name     string `json:"name"`
}

// parseCodewhaleDoc extracts the last turn and the stated workspace from
// a decoded session document.
func parseCodewhaleDoc(data []byte) (role session.Role, tool, text, stated string, ok bool) {
	var doc codewhaleDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return session.RoleUnknown, "", "", "", false
	}
	if ws, isStr := doc.Metadata["workspace"].(string); isStr && filepath.IsAbs(ws) && isDir(ws) {
		stated = ws
	}
	if len(doc.Messages) == 0 {
		return session.RoleUnknown, "", "", stated, true
	}
	last := doc.Messages[len(doc.Messages)-1]
	switch strings.ToLower(last.Role) {
	case "user", "human":
		role = session.RoleUser
	case "assistant", "model", "agent":
		role = session.RoleAssistant
	}
	tool, text = codewhaleContent(last.Role, last.Content)
	text = sanitize(text)
	if len(text) > 200 {
		text = truncRunes(text, 200)
	}
	return role, tool, text, stated, true
}

// codewhaleContent interprets one message's content, which is either a
// bare string or a list of typed blocks. Malformed content costs only the
// text and tool: the role still classifies the turn.
func codewhaleContent(roleStr string, content json.RawMessage) (tool, text string) {
	var blocks []codewhaleBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		for _, b := range blocks {
			switch b.Type {
			case "tool_use", "tool_call", "function_call":
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
	// A bare string content is a prompt on a user turn and a reply on an
	// assistant one; only the assistant's says anything about the agent.
	var plain string
	if err := json.Unmarshal(content, &plain); err == nil {
		switch strings.ToLower(roleStr) {
		case "assistant", "model", "agent":
			return "", plain
		}
	}
	return "", ""
}

// Evidence reports how one session log's workspace was resolved, and
// from which read window. Intended for `agent-monitor --explain`.
type Evidence struct {
	ID      string
	Path    string
	Size    int64
	Window  string // "head" or "tail": which window supplied the answer
	Signal  string // "metadata key", "git checkout", "frequent path", or "none"
	Key     string // key name when Signal is "metadata key"
	Cwd     string // resolved workspace, "" when unresolved
	Project string // encoded project-dir form, "" when unresolved
	// Candidates are the most-mentioned absolute paths in the window
	// that produced the answer. A weak "frequent path" inference is
	// only auditable if the runner-up is visible next to it.
	Candidates []pathCount

	// Last-turn shape, parsed from the same tail window the scanner
	// uses. This is what the classifier turns into ACTIVE/IDLE, so
	// printing it answers "why does this card say IDLE?".
	LastRole string
	LastTool string
	LastText string

	// Shapes describes the key structure of the most recent records —
	// key names and short type/role values only, never message text.
	// Needed because the store's schema is Codewhale's, and a card that
	// can't classify is unreadable without seeing what was actually
	// written.
	Shapes []string

	// ScannedTail is how many bytes the last-turn scan had to read
	// backwards. A large value means records are oversized, which is
	// what a fixed tail window gets wrong.
	ScannedTail int64
}

// ExplainCodewhale resolves a log's workspace and reports the evidence.
// It calls the same resolver, over the same bounded windows, as Scan —
// so a diagnostic can never disagree with the card it explains.
func ExplainCodewhale(path, id string) Evidence {
	ev := Evidence{ID: id, Path: path, Signal: "none"}
	f, err := os.Open(path)
	if err != nil {
		return ev
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ev
	}
	ev.Size = info.Size()
	head := readWindow(f, 0, ev.Size, codewhaleHeadBytes)
	tailStart := ev.Size - codewhaleTailFloor
	if tailStart < 0 {
		tailStart = 0
	}
	tail := readWindow(f, tailStart, ev.Size, codewhaleTailFloor)

	// Prefer the document parse; it is the only reading that gets the
	// LAST turn and the stated workspace right.
	var stated string
	data := readWindow(f, 0, ev.Size, codewhaleTailCeiling)
	if int64(len(data)) == ev.Size {
		if role, tool, text, ws, ok := parseCodewhaleDoc(data); ok {
			stated = ws
			ev.LastRole = string(role)
			ev.LastTool, ev.LastText = tool, text
			ev.ScannedTail = ev.Size
			ev.Shapes = docShape(data)
		}
	}
	if ev.LastRole == "" {
		role, tool, text, scanned := lastTurnBackward(f, ev.Size)
		ev.LastRole, ev.LastTool, ev.LastText = string(role), tool, text
		ev.ScannedTail = scanned
		ev.LastRole = string(role)
	}
	if ev.LastRole == "" {
		ev.LastRole = "(unknown)"
	}

	hit := resolveCWD(head, tail, stated)
	ev.Signal, ev.Key, ev.Window = hit.signal, hit.key, hit.window
	ev.Candidates = hit.candTop
	if hit.path == "" {
		return ev
	}
	ev.Cwd = hit.path
	ev.Project = encodeCodewhaleCWD(ev.Cwd)
	return ev
}

// parseCodewhaleCWD reads both bounded windows of a log and resolves the
// workspace. Shares ExplainCodewhale's resolver so the card and the
// diagnostic are the same computation.
func parseCodewhaleCWD(path string) string {
	return ExplainCodewhale(path, "").Cwd
}

// parseCodewhaleTail reads the session document and returns the last
// turn's role, tool and text. Falls back to the backward line scan when
// the document can't be decoded (for example a log larger than the read
// ceiling).
func parseCodewhaleTail(path string, tailBytes int64) (session.Role, string, string) {
	f, err := os.Open(path)
	if err != nil {
		return session.RoleUnknown, "", ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return session.RoleUnknown, "", ""
	}
	size := info.Size()
	if data := readWindow(f, 0, size, codewhaleTailCeiling); int64(len(data)) == size {
		if role, tool, text, _, ok := parseCodewhaleDoc(data); ok {
			return role, tool, text
		}
	}
	role, tool, text, _ := lastTurnBackward(f, size)
	return role, tool, text
}

// readWindow returns up to `want` bytes of f starting at `start`.
func readWindow(f *os.File, start, end int64, want int64) []byte {
	if start < 0 {
		start = 0
	}
	if end > start+want {
		end = start + want
	}
	if end <= start {
		return nil
	}
	buf := make([]byte, end-start)
	n, _ := f.ReadAt(buf, start)
	return buf[:n]
}

// codewhaleKeyCandidates are the keys agent runtimes use for "the
// workspace this session runs in". Checked before the path heuristics.
var codewhaleKeyCandidates = []string{
	"cwd",
	"working_directory",
	"workspace",
	"workspace_dir",
	"workspace_path",
	"workspace_root",
	"project_dir",
	"project_path",
	"root",
	"dir",
}

// codewhaleCWD extracts an absolute existing directory from one buffer,
// strong signals first. Thin wrapper kept for tests and one-window
// callers; multi-window callers must use resolveCWD.
func codewhaleCWD(buf []byte) string {
	return resolveCWD(buf, nil, "").path
}

// cwdHit records not just the resolved workspace but the evidence that
// produced it. The dashboard's core job is parsing stores owned by other
// programs, so "why did this card say Documents?" needs an answer that
// doesn't require reading the log by hand.
type cwdHit struct {
	path    string
	signal  string // "metadata key", "git checkout", "frequent path", or "none"
	key     string // the key name, when signal is "metadata key"
	window  string // "head" or "tail": which window produced the answer
	candTop []pathCount
}

// pathCount is one absolute path seen in a read window, with how often
// it appeared. Reported by --explain so a weak inference is visible as
// weak instead of looking like a fact.
type pathCount struct {
	Path  string
	Count int
}

// resolveCWD is the single source of truth for workspace resolution.
// Both the scanner and --explain call it, so a diagnostic can never
// disagree with the card it is explaining.
//
// Precedence, in order:
//
//  1. a metadata key naming a git checkout (the log states it, and it is
//     the unit the dashboard reasons about);
//  2. a checkout the log demonstrably worked in, when the stated
//     workspace merely contains it — `workspace: ~/Documents` with every
//     touched file under ~/Documents/repo is a launch directory, not a
//     project, and "Documents" on the card helps nobody;
//  3. the stated workspace as-is;
//  4. the busiest checkout mentioned anywhere;
//  5. the most-mentioned existing directory.
func resolveCWD(head, tail []byte, stated string) cwdHit {
	type window struct {
		name string
		buf  []byte
	}
	windows := []window{{"head", head}, {"tail", tail}}

	var meta cwdHit
	if stated != "" {
		meta = cwdHit{path: stated, signal: "metadata key", key: "metadata.workspace", window: "doc"}
	}
	for _, w := range windows {
		if meta.path != "" || len(w.buf) == 0 {
			break
		}
		if p, key := cwdFromKnownKeys(w.buf); p != "" {
			meta = cwdHit{path: p, signal: "metadata key", key: key, window: w.name}
		}
	}

	var repo string
	var repoWindow string
	for _, w := range windows {
		if len(w.buf) == 0 {
			continue
		}
		if p := repoByMention(w.buf); p != "" {
			repo, repoWindow = p, w.name
			break
		}
	}

	for _, w := range windows {
		if len(w.buf) == 0 {
			continue
		}
		if hit := pickCWD(meta, repo, repoWindow, w.buf); hit.path != "" {
			return hit
		}
	}
	return cwdHit{signal: "none", candTop: topPaths(head, 6)}
}

// pickCWD applies the precedence order for one window.
func pickCWD(meta cwdHit, repo, repoWindow string, buf []byte) cwdHit {
	switch {
	case meta.path != "" && isGitRepo(meta.path):
		return cwdHit{path: meta.path, signal: "metadata key", key: meta.key, window: meta.window, candTop: topPaths(buf, 6)}
	case meta.path != "" && repo != "" && withinDir(meta.path, repo):
		return cwdHit{path: repo, signal: "git checkout", window: repoWindow, candTop: topPaths(buf, 6)}
	case meta.path != "":
		return cwdHit{path: meta.path, signal: "metadata key", key: meta.key, window: meta.window, candTop: topPaths(buf, 6)}
	case repo != "":
		return cwdHit{path: repo, signal: "git checkout", window: repoWindow, candTop: topPaths(buf, 6)}
	}
	if p := codewhaleCWDWeak(buf); p != "" {
		return cwdHit{path: p, signal: "frequent path", window: "", candTop: topPaths(buf, 6)}
	}
	return cwdHit{}
}

// withinDir reports whether p is base or lives beneath it.
func withinDir(base, p string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

// topPaths returns the most frequently mentioned absolute paths in a
// window. Diagnostics only; never used to resolve.
func topPaths(buf []byte, n int) []pathCount {
	counts := map[string]int{}
	var order []string
	for _, m := range codewhalePathRe.FindAllSubmatch(buf, 2048) {
		p := string(m[1])
		if !filepath.IsAbs(p) || systemDirs[p] || isAgentStorePath(p) {
			continue
		}
		if _, seen := counts[p]; !seen {
			order = append(order, p)
		}
		counts[p]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	if len(order) > n {
		order = order[:n]
	}
	out := make([]pathCount, 0, len(order))
	for _, p := range order {
		out = append(out, pathCount{Path: p, Count: counts[p]})
	}
	return out
}

// repoByMention returns the git checkout this window talks about most.
//
// Logs rarely name the workspace directly; they name files inside it, a
// `.git/logs/HEAD` path, or a sibling repo. So every candidate path is
// walked up to its containing checkout, and mentions are counted per
// repo rather than per path. The busiest repo is the one the session was
// working on, which is exactly what the card should say.
func repoByMention(buf []byte) string {
	counts := map[string]int{}
	var order []string
	memo := map[string]string{}
	for _, m := range codewhalePathRe.FindAllSubmatch(buf, 256) {
		p := string(m[1])
		if !filepath.IsAbs(p) || systemDirs[p] || isAgentStorePath(p) {
			continue
		}
		repo := containingRepo(p, memo)
		if repo == "" {
			continue
		}
		if counts[repo] == 0 {
			order = append(order, repo)
		}
		counts[repo]++
	}
	best, bestN := "", 0
	for _, r := range order {
		if counts[r] > bestN {
			best, bestN = r, counts[r]
		}
	}
	return best
}

// containingRepo walks from p up to the nearest git checkout, memoizing
// every directory it visits. Without the memo a window naming 200 files
// in one repo would repeat the same ascent 200 times per tick.
func containingRepo(p string, memo map[string]string) string {
	dir := p
	if !isDir(dir) {
		dir = filepath.Dir(dir)
	}
	var visited []string
	for i := 0; i < 32; i++ {
		if repo, ok := memo[dir]; ok {
			for _, v := range visited {
				memo[v] = repo
			}
			return repo
		}
		if isGitRepo(dir) {
			memo[dir] = dir
			for _, v := range visited {
				memo[v] = dir
			}
			return dir
		}
		visited = append(visited, dir)
		parent := filepath.Dir(dir)
		if parent == dir || parent == "/" {
			break
		}
		dir = parent
	}
	for _, v := range visited {
		memo[v] = ""
	}
	return ""
}

// gitCheckoutIn returns the checkout this window mentions most.
func gitCheckoutIn(buf []byte) string {
	return repoByMention(buf)
}

func cwdFromKnownKeys(buf []byte) (string, string) {
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		// A key is semantic evidence, but a key can also appear inside a
		// tool payload (a file's `dir`, a repo's `root`). Stat keeps a
		// file path out of the workspace slot.
		for _, k := range codewhaleKeyCandidates {
			if v := jsonFindString(rec, k); filepath.IsAbs(v) && isDir(v) {
				return v, k
			}
		}
	}
	// Single-document logs (pretty-printed or one giant object) have no
	// usable line boundaries. Parsing the whole window costs nothing
	// when the window is JSONL, because that unmarshal fails instantly.
	var doc any
	if err := json.Unmarshal(bytes.TrimSpace(buf), &doc); err == nil {
		for _, k := range codewhaleKeyCandidates {
			if v := jsonFindString(doc, k); filepath.IsAbs(v) && isDir(v) {
				return v, k
			}
		}
	}
	return "", ""
}

// systemDirs are absolute paths that appear in nearly every log but are
// never the user's workspace.
var systemDirs = map[string]bool{
	"/usr": true, "/bin": true, "/sbin": true, "/etc": true,
	"/var": true, "/tmp": true, "/private": true, "/dev": true,
	"/System": true, "/Library": true, "/Applications": true,
	"/opt": true, "/Users/Shared": true,
}

// agentStoreMarkers are path segments belonging to agent runtimes
// themselves. A session log mentions its own store constantly, and those
// paths are never the user's workspace.
var agentStoreMarkers = []string{"/.codewhale/", "/.cursor/", "/.claude/"}

func isAgentStorePath(p string) bool {
	for _, m := range agentStoreMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

// codewhaleCWDWeak is the schema-free fallback: quoted absolute paths
// that exist on disk. The most frequently mentioned directory wins,
// because a workspace recurs in every turn's context while incidental
// paths appear once.
func codewhaleCWDWeak(buf []byte) string {
	counts := map[string]int{}
	var order []string
	for _, m := range codewhalePathRe.FindAllSubmatch(buf, 512) {
		p := string(m[1])
		if !filepath.IsAbs(p) || systemDirs[p] || isAgentStorePath(p) {
			continue
		}
		if _, seen := counts[p]; !seen && isDir(p) {
			order = append(order, p)
		}
		counts[p]++
	}
	best, bestN := "", 0
	for _, p := range order {
		if counts[p] > bestN {
			best, bestN = p, counts[p]
		}
	}
	return best
}

// codewhaleLastTurn walks the tail backwards and returns the role, tool
// name, and text of the newest record that carries a role. A bare tool
// call has no role of its own in Codewhale-style logs, but it is still
// the assistant acting, so it counts as an assistant turn.
func codewhaleLastTurn(buf []byte) (session.Role, string, string) {
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		role := codewhaleRole(rec)
		tool := jsonFindTool(rec)
		if role == session.RoleUnknown {
			if tool == "" {
				continue
			}
			role = session.RoleAssistant
		}
		text := sanitize(jsonFindText(rec))
		if len(text) > 200 {
			text = truncRunes(text, 200)
		}
		return role, tool, text
	}
	return session.RoleUnknown, "", ""
}

func codewhaleRole(rec any) session.Role {
	switch strings.ToLower(jsonFindString(rec, "role", "author", "speaker")) {
	case "user", "human":
		return session.RoleUser
	case "assistant", "model", "agent":
		return session.RoleAssistant
	}
	return session.RoleUnknown
}

// toolCallTypes are the record/content type discriminators that mean
// "the assistant is invoking a tool". Matched exactly so a tool *result*
// record ("function_call_output") can't be mistaken for a call.
var toolCallTypes = map[string]bool{
	"tool_use":         true,
	"tool_call":        true,
	"function_call":    true,
	"custom_tool_call": true,
	"local_shell_call": true,
	"mcp_tool_call":    true,
}

// jsonFindTool returns the tool name on a tool-call record, if any.
// The outer record type is often generic ("response_item") while the
// call discriminator sits in the payload, so every "type" value in the
// record is considered — not just the shallowest one.
func jsonFindTool(rec any) string {
	name := jsonFindString(rec, "name", "tool_name")
	if name == "" {
		return ""
	}
	for _, t := range jsonFindAllStrings(rec, "type") {
		if toolCallTypes[strings.ToLower(t)] {
			return name
		}
	}
	return ""
}

// jsonFindText pulls message text out of a record. Handles both a plain
// string content and the block forms (`{type:text,text:...}`) used by
// Claude/Cursor/Codex-shaped runtimes.
func jsonFindText(rec any) string {
	if v, ok := jsonFindValue(rec, "content", "text", "output_text", "input_text"); ok {
		var parts []string
		collectText(v, &parts)
		return strings.Join(parts, " ")
	}
	return ""
}

func collectText(v any, parts *[]string) {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			*parts = append(*parts, s)
		}
	case []any:
		for _, item := range t {
			collectText(item, parts)
		}
	case map[string]any:
		for _, k := range []string{"text", "output_text", "input_text", "content"} {
			if child, ok := t[k]; ok {
				collectText(child, parts)
				return
			}
		}
	}
}

// jsonFindString returns the first string stored under one of names,
// searching breadth-first so shallow metadata wins over deeply nested
// tool payloads.
func jsonFindString(v any, names ...string) string {
	found, ok := jsonFindValue(v, names...)
	if !ok {
		return ""
	}
	s, _ := found.(string)
	return s
}

func jsonFindValue(v any, names ...string) (any, bool) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	queue := []any{v}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		m, ok := cur.(map[string]any)
		if !ok {
			if list, ok := cur.([]any); ok && len(queue) < 4096 {
				queue = append(queue, list...)
			}
			continue
		}
		for k, child := range m {
			if want[k] {
				if s, ok := child.(string); ok && strings.TrimSpace(s) != "" {
					return s, true
				}
			}
		}
		for _, child := range m {
			switch child.(type) {
			case map[string]any, []any:
				if len(queue) < 4096 {
					queue = append(queue, child)
				}
			}
		}
	}
	return nil, false
}

// jsonFindAllStrings returns every string stored under one of names,
// in breadth-first order.
func jsonFindAllStrings(v any, names ...string) []string {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []string
	queue := []any{v}
	for len(queue) > 0 && len(out) < 64 {
		cur := queue[0]
		queue = queue[1:]
		m, ok := cur.(map[string]any)
		if !ok {
			if list, ok := cur.([]any); ok && len(queue) < 4096 {
				queue = append(queue, list...)
			}
			continue
		}
		for k, child := range m {
			if want[k] {
				if s, ok := child.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
		}
		for _, child := range m {
			switch child.(type) {
			case map[string]any, []any:
				if len(queue) < 4096 {
					queue = append(queue, child)
				}
			}
		}
	}
	return out
}

// encodeCodewhaleCWD turns an absolute workspace path into the same
// encoded project-dir form Cursor uses, so every downstream helper
// (DecodeProjectPath, SlugFromEncoded, worktree dedupe) works unchanged
// for Codewhale cards.
func encodeCodewhaleCWD(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	return EncodeForCursor(strings.TrimPrefix(cwd, "/"))
}

// sortCandidatesNewestFirst keeps the merged Cursor + Codewhale list in
// the order the card viewport expects.
func sortCandidatesNewestFirst(cands []candidate) {
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].mtime > cands[j].mtime })
}
