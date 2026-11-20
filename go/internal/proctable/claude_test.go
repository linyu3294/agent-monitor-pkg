package proctable

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

func TestLooksLikeClaude(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"/opt/homebrew/bin/claude", true},
		{"/Users/u/.local/bin/claude --permission-mode acceptEdits", true},
		{"/Users/u/.local/share/claude/versions/2.0.37", true},
		{"/usr/local/bin/node /usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js", true},
		// The desktop app is not the CLI, and a process that merely
		// mentions the word is not the CLI either.
		{"/Applications/Claude.app/Contents/MacOS/Claude", false},
		{"/opt/homebrew/bin/claude-code-helper", false},
		{"grep claude /tmp/log", false},
		{"bash -c 'echo claude'", false},
		{"/opt/homebrew/bin/codewhale", false},
	}
	for _, c := range cases {
		if got := looksLikeClaude([]byte(c.cmd)); got != c.want {
			t.Errorf("looksLikeClaude(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestBindClaudeSessions(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	started := time.Now().Add(-time.Hour)
	old := started.Add(-2 * time.Hour).Unix()
	fresh := started.Add(time.Minute).Unix()

	t.Run("binds a single plausible session by workspace", func(t *testing.T) {
		tbl := &Table{
			byChat:      map[string]Process{},
			claudeProcs: []claudeProc{{pid: 4242, state: 'R', started: started, cwd: dir}},
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{
			dir: {{ID: "session-a", Mtime: fresh}},
		})

		got, ok := tbl.byChat["session-a"]
		if !ok {
			t.Fatal("session-a was not bound")
		}
		if got.PID != 4242 || got.State != 'R' {
			t.Fatalf("bound process = %+v, want pid 4242 state R", got)
		}
	})

	// The reviewer's case: a finished session in the same directory has a
	// newer transcript than the live session's last write. Binding the
	// newest would hand `r` and `x` the wrong card.
	t.Run("refuses when a finished session is also plausible", func(t *testing.T) {
		tbl := &Table{
			byChat:      map[string]Process{},
			claudeProcs: []claudeProc{{pid: 4242, state: 'S', started: started, cwd: dir}},
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{
			dir: {{ID: "live", Mtime: fresh}, {ID: "newer-but-finished", Mtime: fresh + 600}},
		})
		if len(tbl.byChat) != 0 {
			t.Fatalf("bound %v, want nothing for an ambiguous workspace", tbl.byChat)
		}
	})

	// A session that ended before this process started cannot be its
	// session, so it must not create ambiguity.
	t.Run("ignores a session older than the process", func(t *testing.T) {
		tbl := &Table{
			byChat:      map[string]Process{},
			claudeProcs: []claudeProc{{pid: 4242, state: 'R', started: started, cwd: dir}},
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{
			dir: {{ID: "current", Mtime: fresh}, {ID: "yesterday", Mtime: old}},
		})
		if got := tbl.byChat["current"].PID; got != 4242 {
			t.Fatalf("current bound to %d, want 4242 (%v)", got, tbl.byChat)
		}
		if _, bound := tbl.byChat["yesterday"]; bound {
			t.Fatalf("a pre-existing session was bound: %v", tbl.byChat)
		}
	})

	t.Run("ignores an unknown workspace", func(t *testing.T) {
		tbl := &Table{
			byChat:      map[string]Process{},
			claudeProcs: []claudeProc{{pid: 1, state: 'S', started: started, cwd: dir}},
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{
			other: {{ID: "session-b", Mtime: fresh}},
		})
		if len(tbl.byChat) != 0 {
			t.Fatalf("bound %v, want nothing", tbl.byChat)
		}
	})

	t.Run("ignores a process with no cwd", func(t *testing.T) {
		tbl := &Table{
			byChat:      map[string]Process{},
			claudeProcs: []claudeProc{{pid: 1, state: 'S', started: started}},
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{
			dir: {{ID: "session-a", Mtime: fresh}},
		})
		if len(tbl.byChat) != 0 {
			t.Fatalf("bound %v, want nothing", tbl.byChat)
		}
	})

	t.Run("refuses an ambiguous workspace with an unknown start time", func(t *testing.T) {
		tbl := &Table{
			byChat: map[string]Process{},
			claudeProcs: []claudeProc{
				{pid: 1, state: 'S', cwd: dir},
				{pid: 2, state: 'S', cwd: dir},
			},
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{
			dir: {{ID: "session-a", Mtime: fresh}},
		})
		if len(tbl.byChat) != 0 {
			t.Fatalf("bound %v, want nothing for an ambiguous workspace", tbl.byChat)
		}
	})

	t.Run("does not overwrite another family's binding", func(t *testing.T) {
		tbl := &Table{
			byChat:      map[string]Process{"session-a": {PID: 999}},
			claudeProcs: []claudeProc{{pid: 4242, state: 'R', started: started, cwd: dir}},
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{
			dir: {{ID: "session-a", Mtime: fresh}},
		})
		if got := tbl.byChat["session-a"].PID; got != 999 {
			t.Fatalf("PID = %d, want the pre-existing 999", got)
		}
	})
}

func TestClaudeWorkspaceIndex(t *testing.T) {
	dir := t.TempDir()
	sessions := []session.Session{
		{ID: "older", Source: session.SourceClaude, CWDHint: dir, Mtime: 100},
		{ID: "newer", Source: session.SourceClaude, CWDHint: dir, Mtime: 200},
		// Other sources and unlabelled sessions contribute nothing.
		{ID: "cursor", Source: session.SourceCursor, CWDHint: t.TempDir(), Mtime: 300},
		{ID: "nolabel", Source: session.SourceClaude, Mtime: 400},
	}
	idx := ClaudeWorkspaceIndex(sessions)
	refs := idx[filepath.Clean(dir)]
	if len(refs) != 2 {
		t.Fatalf("index[%q] = %+v, want both sessions in the directory", dir, refs)
	}
	ids := map[string]bool{}
	for _, r := range refs {
		ids[r.ID] = true
	}
	if !ids["older"] || !ids["newer"] {
		t.Fatalf("index = %+v, want older and newer", refs)
	}
	if len(idx) > 2 {
		t.Fatalf("index has %d entries, want only Claude workspaces: %v", len(idx), idx)
	}
}

// lsofCwdLine builds the `cwd` descriptor row lsof prints for every
// process it is asked about.
func lsofCwdLine(cmd string, pid int, dir string) string {
	return cmd + " " + strconv.Itoa(pid) + " user cwd DIR 1,2 4096 123456 " + dir
}

func TestBindFromLsofClaude(t *testing.T) {
	const id = "8b7c6d5e-4f3a-4b2c-9d1e-0a1b2c3d4e5f"
	store := "/Users/u/.claude/projects"
	dir := t.TempDir()

	t.Run("an open transcript names its session exactly", func(t *testing.T) {
		out := strings.Join([]string{
			"COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME",
			lsofCwdLine("claude", 4242, dir),
			lsofLine("claude", 4242, store+"/-tmp-repo/"+id+".jsonl"),
		}, "\n")

		tbl := &Table{byChat: map[string]Process{}, claudeDir: store}
		tbl.bindFromLsof([]byte(out), map[int]agentProc{
			4242: {pid: 4242, state: 'S', claude: true},
		})

		got, ok := tbl.byChat[id]
		if !ok || got.PID != 4242 {
			t.Fatalf("byChat = %v, want %s bound to 4242", tbl.byChat, id)
		}
		if len(tbl.claudeProcs) != 1 || !tbl.claudeProcs[0].exact {
			t.Fatalf("claudeProcs = %+v, want one exact binding", tbl.claudeProcs)
		}
		// The workspace join must not move an exact binding elsewhere.
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{dir: {{ID: "some-other-session", Mtime: time.Now().Unix()}}})
		if got := tbl.byChat[id].PID; got != 4242 {
			t.Fatalf("exact binding moved to %d", got)
		}
		if _, moved := tbl.byChat["some-other-session"]; moved {
			t.Fatal("an exact-bound process was also bound by workspace")
		}
	})

	t.Run("a subagent transcript does not bind", func(t *testing.T) {
		out := strings.Join([]string{
			"COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME",
			lsofLine("claude", 4242, store+"/-tmp-repo/"+id+"/subagents/agent-abc.jsonl"),
			lsofLine("claude", 4242, store+"/-tmp-repo/sessions-index.json"),
		}, "\n")

		tbl := &Table{byChat: map[string]Process{}, claudeDir: store}
		tbl.bindFromLsof([]byte(out), map[int]agentProc{
			4242: {pid: 4242, state: 'S', claude: true},
		})
		if len(tbl.byChat) != 0 {
			t.Fatalf("byChat = %v, want nothing bound", tbl.byChat)
		}
	})

	t.Run("a cwd row is kept for the workspace join", func(t *testing.T) {
		out := strings.Join([]string{
			"COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME",
			lsofCwdLine("claude", 4242, dir),
		}, "\n")

		tbl := &Table{byChat: map[string]Process{}, claudeDir: store}
		tbl.bindFromLsof([]byte(out), map[int]agentProc{
			4242: {pid: 4242, state: 'S', claude: true},
		})
		if len(tbl.claudeProcs) != 1 || tbl.claudeProcs[0].cwd != dir {
			t.Fatalf("claudeProcs = %+v, want pid 4242 cwd %q", tbl.claudeProcs, dir)
		}
		tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{dir: {{ID: "session-a", Mtime: time.Now().Unix()}}})
		if tbl.byChat["session-a"].PID != 4242 {
			t.Fatalf("workspace join did not bind: %v", tbl.byChat)
		}
	})
}

// lsof does not promise that the cwd row comes first. When the
// transcript row lands first, the later cwd row must not add a second,
// non-exact entry that could bind a different session to the same pid.
func TestBindFromLsofClaudeTranscriptBeforeCwd(t *testing.T) {
	const id = "8b7c6d5e-4f3a-4b2c-9d1e-0a1b2c3d4e5f"
	store := "/Users/u/.claude/projects"
	dir := t.TempDir()

	out := strings.Join([]string{
		"COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME",
		lsofLine("claude", 4242, store+"/-tmp-repo/"+id+".jsonl"),
		lsofCwdLine("claude", 4242, dir),
	}, "\n")

	tbl := &Table{byChat: map[string]Process{}, claudeDir: store}
	tbl.bindFromLsof([]byte(out), map[int]agentProc{
		4242: {pid: 4242, state: 'R', claude: true},
	})

	if len(tbl.claudeProcs) != 1 || !tbl.claudeProcs[0].exact {
		t.Fatalf("claudeProcs = %+v, want one exact entry", tbl.claudeProcs)
	}
	// A newer transcript in the same directory must not be bound too.
	tbl.BindClaudeSessions(map[string][]ClaudeSessionRef{dir: {{ID: "decoy-session", Mtime: time.Now().Unix()}}})
	if _, bound := tbl.byChat["decoy-session"]; bound {
		t.Fatalf("an exact-bound pid was also bound by workspace: %v", tbl.byChat)
	}
}

// ps `etime` is the only start-time source, so its three observed shapes
// must parse exactly and a malformed value must not invent a start time.
func TestParseElapsed(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"00:42", 42 * time.Second},
		{"12:34", 12*time.Minute + 34*time.Second},
		{"01:02:03", time.Hour + 2*time.Minute + 3*time.Second},
		{"3-04:05:06", 3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second},
		{" 07:08 ", 7*time.Minute + 8*time.Second},
		{"", 0},
		{"garbage", 0},
		{"1:2:3:4", 0},
	}
	for _, c := range cases {
		if got := parseElapsed(c.in); got != c.want {
			t.Errorf("parseElapsed(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestClaudeIDFromOpenPath(t *testing.T) {
	const id = "8b7c6d5e-4f3a-4b2c-9d1e-0a1b2c3d4e5f"
	store := "/Users/u/.claude/projects"
	cases := []struct {
		name string
		path string
		want string
	}{
		{"session transcript", store + "/-Users-u-repo/" + id + ".jsonl", id},
		{"subagent transcript", store + "/-Users-u-repo/" + id + "/subagents/agent-x.jsonl", ""},
		{"index file", store + "/-Users-u-repo/sessions-index.json", ""},
		{"non-uuid name", store + "/-Users-u-repo/notes.jsonl", ""},
		{"outside the store", "/tmp/" + id + ".jsonl", ""},
		{"empty store", store + "/-Users-u-repo/" + id + ".jsonl", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := store
			if c.name == "empty store" {
				dir = ""
			}
			if got := claudeIDFromOpenPath(c.path, dir); got != c.want {
				t.Fatalf("claudeIDFromOpenPath(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}
