// Package scanner walks the Cursor projects directory and produces
// Session records for every transcript whose mtime is recent enough to
// surface in the dashboard.
//
// Performance contract: a quiescent dashboard (no transcript advanced
// since last tick) must do approximately zero work. We achieve this by
// caching (path -> mtime, parsed metadata) across ticks and re-parsing
// only files whose mtime advanced.
package scanner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// Options controls a Scanner instance.
type Options struct {
	ProjectsDir   string        // default: $HOME/.cursor/projects
	IdleThreshold time.Duration // skip transcripts mtime older than now-IdleThreshold
	MaxCards      int           // hard upper bound on returned sessions
	TailBytes     int64         // bytes to read from the tail of each transcript (default 8192)
}

// Scanner scans the on-disk project tree and produces Session records.
type Scanner struct {
	opts  Options
	cache map[string]cacheEntry // key: transcript path
}

type cacheEntry struct {
	mtime    int64
	role     session.Role
	tool     string
	text     string
	cwdHint  string
}

// New returns a Scanner with sensible defaults filled in.
func New(opts Options) *Scanner {
	if opts.ProjectsDir == "" {
		home, _ := os.UserHomeDir()
		opts.ProjectsDir = filepath.Join(home, ".cursor", "projects")
	}
	if opts.IdleThreshold == 0 {
		opts.IdleThreshold = 20 * time.Minute
	}
	if opts.MaxCards == 0 {
		opts.MaxCards = 50
	}
	if opts.TailBytes == 0 {
		opts.TailBytes = 8192
	}
	return &Scanner{opts: opts, cache: make(map[string]cacheEntry)}
}

// Scan walks the projects dir and returns sessions sorted newest first.
// Errors during walking are tolerated — a missing or unreadable directory
// just contributes nothing. Per-file parse errors are also tolerated; the
// session is returned with empty role/tool/text rather than dropped.
func (s *Scanner) Scan() []session.Session {
	cutoff := time.Now().Add(-s.opts.IdleThreshold).Unix()

	root := s.opts.ProjectsDir
	if _, err := os.Stat(root); err != nil {
		return nil
	}

	type candidate struct {
		path  string
		mtime int64
	}
	var cands []candidate

	// We expect exactly four levels under root:
	//   <root>/<project>/agent-transcripts/<id>/<id>.jsonl
	// so a hand-rolled descent is faster than filepath.Walk and avoids
	// recursing into unrelated directories.
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, pe := range projects {
		if !pe.IsDir() {
			continue
		}
		// Worktree-backed project dirs always surface, regardless of
		// IdleThreshold — the dashboard's contract is "1 card per
		// worktree, always", so an idle agent on a worktree must not
		// drop off the list. Primary-checkout sessions still age out
		// at the cutoff so the dashboard doesn't fill up with weeks
		// of stale chats.
		isWorktree := strings.Contains(pe.Name(), "__worktrees__") ||
			strings.Contains(pe.Name(), "-worktrees-")
		atRoot := filepath.Join(root, pe.Name(), "agent-transcripts")
		ids, err := os.ReadDir(atRoot)
		if err != nil {
			continue
		}
		for _, ide := range ids {
			if !ide.IsDir() {
				continue
			}
			id := ide.Name()
			f := filepath.Join(atRoot, id, id+".jsonl")
			info, err := os.Stat(f)
			if err != nil {
				continue
			}
			m := info.ModTime().Unix()
			if !isWorktree && m < cutoff {
				continue
			}
			cands = append(cands, candidate{f, m})
		}
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].mtime > cands[j].mtime })
	if len(cands) > s.opts.MaxCards {
		cands = cands[:s.opts.MaxCards]
	}

	out := make([]session.Session, 0, len(cands))
	for _, c := range cands {
		sess := session.Session{
			TranscriptPath: c.path,
			Mtime:          c.mtime,
			ID:             chatIDFromPath(c.path),
		}
		sess.ProjectDir = session.ProjectDirOf(c.path)
		sess.Project = session.ProjectLabel(sess.ProjectDir)
		sess.WorktreeSlug = worktreeSlugFor(sess.ProjectDir)

		// Mtime cache: re-parse only when the file has actually changed.
		entry, hit := s.cache[c.path]
		if hit && entry.mtime == c.mtime {
			sess.LastRole = entry.role
			sess.LastTool = entry.tool
			sess.LastText = entry.text
			sess.CWDHint = entry.cwdHint
		} else {
			role, tool, text := s.parseTail(c.path)
			cwd := entry.cwdHint
			if !hit {
				// First time seeing this file — extract the cwd hint
				// once. It's stable for the life of a session.
				cwd = parseCWDHint(c.path)
			}
			sess.LastRole = role
			sess.LastTool = tool
			sess.LastText = text
			sess.CWDHint = cwd
			s.cache[c.path] = cacheEntry{
				mtime:   c.mtime,
				role:    role,
				tool:    tool,
				text:    text,
				cwdHint: cwd,
			}
		}
		out = append(out, sess)
	}

	// GC: drop cache entries for paths we didn't see this scan, so the
	// map doesn't grow unbounded over a long-running dashboard.
	seen := make(map[string]struct{}, len(out))
	for _, sess := range out {
		seen[sess.TranscriptPath] = struct{}{}
	}
	for k := range s.cache {
		if _, ok := seen[k]; !ok {
			delete(s.cache, k)
		}
	}

	return out
}

// chatIDFromPath extracts the UUID from a transcript path of the shape
// `.../<id>/<id>.jsonl`.
func chatIDFromPath(p string) string {
	base := filepath.Base(p)
	return strings.TrimSuffix(base, ".jsonl")
}

// parseTail reads the last TailBytes of the file, finds the last complete
// JSONL object, and extracts role + last tool_use name + last text.
func (s *Scanner) parseTail(path string) (session.Role, string, string) {
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
	read := s.opts.TailBytes
	if read > size {
		read = size
	}
	if read <= 0 {
		return session.RoleUnknown, "", ""
	}
	buf := make([]byte, read)
	if _, err := f.ReadAt(buf, size-read); err != nil {
		return session.RoleUnknown, "", ""
	}

	// Walk newline-separated lines; keep the last one that parses.
	var last []byte
	scn := bufio.NewScanner(bytes.NewReader(buf))
	scn.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scn.Scan() {
		line := bytes.TrimSpace(scn.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		// Snapshot the line; bufio.Scanner reuses its buffer.
		last = append(last[:0], line...)
	}

	if len(last) == 0 {
		return session.RoleUnknown, "", ""
	}

	var entry struct {
		Role    string `json:"role"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(last, &entry); err != nil {
		return session.RoleUnknown, "", ""
	}

	role := session.Role(entry.Role)
	var tool, text string
	for _, c := range entry.Message.Content {
		switch c.Type {
		case "tool_use":
			if c.Name != "" {
				tool = c.Name
			}
		case "text":
			if c.Text != "" {
				text = c.Text
			}
		}
	}
	text = sanitize(text)
	if len(text) > 200 {
		// Truncate by runes, not bytes, to avoid splitting a multi-byte
		// codepoint across the cutoff.
		text = truncRunes(text, 200)
	}
	return role, tool, text
}

var cdRegex = regexp.MustCompile(`cd +([^ &;|]+)`)

// parseCWDHint reads the head of a transcript and extracts the first `cd
// <path>` argument from the first Shell tool_use input. Used as a fallback
// when the project dir alone doesn't tell us where the worktree lives.
func parseCWDHint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	const head = 64 * 1024
	buf := make([]byte, head)
	n, _ := f.Read(buf)
	if n <= 0 {
		return ""
	}
	scn := bufio.NewScanner(bytes.NewReader(buf[:n]))
	scn.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scn.Scan() {
		line := bytes.TrimSpace(scn.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var entry struct {
			Role    string `json:"role"`
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Command string `json:"command"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Role != "assistant" {
			continue
		}
		for _, c := range entry.Message.Content {
			if c.Type != "tool_use" || c.Name != "Shell" {
				continue
			}
			if m := cdRegex.FindStringSubmatch(c.Input.Command); len(m) >= 2 {
				return m[1]
			}
		}
	}
	return ""
}

// worktreeSlugFor decodes the worktree slug embedded in a project-dir
// basename, if any. Cursor encodes the workspace path with `/` → `-`, so
// `<repo>/.worktrees/<slug>` becomes `<encoded-repo>-worktrees-<encoded-slug>`.
//
// The encoded slug can be ambiguous (both `/` and `__` collapse into `-`),
// so the canonical decode requires resolving against the on-disk
// `.worktrees/` entries of the primary repo. That logic lives in the
// classifier path; here we just surface the encoded tail when it's
// present, which is enough for the card's display label.
func worktreeSlugFor(projectDir string) string {
	switch {
	case strings.Contains(projectDir, "__worktrees__"):
		_, after, _ := strings.Cut(projectDir, "__worktrees__")
		return after
	case strings.Contains(projectDir, "-worktrees-"):
		_, after, _ := strings.Cut(projectDir, "-worktrees-")
		return after
	}
	return ""
}

// DecodeProjectPath best-effort-decodes a project-dir basename back into
// an absolute on-disk path. Returns "" if no candidate path resolves.
//
// Cursor encodes paths by stripping the leading `/` and replacing each `/`
// with `-`. Decoding is ambiguous when the real path contains literal `-`
// (e.g. `trivelta-backend-services`), so we walk down the candidates from
// most-slashed to least-slashed and return the first one that exists as a
// git repo on disk.
func DecodeProjectPath(projectDir string) string {
	enc := projectDir
	// Strip the legacy `__worktrees__<slug>` tail unconditionally.
	if i := strings.Index(enc, "__worktrees__"); i >= 0 {
		enc = enc[:i]
	}
	// Try the full encoded string first; only strip a `-worktrees-<slug>`
	// tail on the retry pass so we don't break repos whose path
	// genuinely contains `-worktrees-`.
	if p := decodeWalk(enc); p != "" {
		return p
	}
	if i := strings.LastIndex(enc, "-worktrees-"); i >= 0 {
		if p := decodeWalk(enc[:i]); p != "" {
			return p
		}
	}
	return ""
}

func decodeWalk(enc string) string {
	if enc == "" {
		return ""
	}
	candidate := "/" + strings.ReplaceAll(enc, "-", "/")
	if isGitRepo(candidate) {
		return candidate
	}
	// Try coarser variants by replacing the rightmost `/` with `-`.
	attempt := candidate
	for strings.Contains(attempt, "/") {
		idx := strings.LastIndex(attempt, "/")
		attempt = attempt[:idx] + "-" + attempt[idx+1:]
		if isGitRepo(attempt) {
			return attempt
		}
	}
	return ""
}

func isGitRepo(path string) bool {
	st, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	// `.git` may be a directory (regular repo) or a file (worktree linked file).
	return st.IsDir() || st.Mode().IsRegular()
}

// SlugFromEncoded resolves an encoded slug (Cursor-flavored: `/` and `__`
// both collapse to `-`) back to the real on-disk worktree slug by scanning
// `<primary>/.worktrees/`. Returns "" on no match.
func SlugFromEncoded(primary, encoded string) string {
	root := filepath.Join(primary, ".worktrees")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		base := e.Name()
		// Mirror Cursor encoding: `__` → `-` and `/` → `-`.
		ee := strings.ReplaceAll(base, "__", "-")
		ee = strings.ReplaceAll(ee, "/", "-")
		if ee == encoded {
			return base
		}
	}
	return ""
}

// truncRunes returns up to n leading runes of s.
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// sanitize replaces transcript-only control characters (newlines, tabs,
// CR) with single spaces so they don't break the card layout.
func sanitize(s string) string {
	if s == "" {
		return s
	}
	repl := func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}
	return strings.Map(repl, s)
}

// fs.WalkDirFunc-shaped helper kept for completeness; not currently used.
var _ fs.WalkDirFunc = func(path string, d fs.DirEntry, err error) error { return err }
