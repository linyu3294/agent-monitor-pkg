package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// writeCodewhaleLog writes a session log in the JSONL shape Codewhale
// uses (session metadata first, one record per turn, tool calls
// interleaved). The reader is deliberately key-tolerant rather than
// schema-pinned, so the fixture exercises the shape rather than a single
// exact struct.
func writeCodewhaleLog(t *testing.T, dir, id string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, id+".json")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newCodewhaleScanner(t *testing.T, projectsDir, codewhaleDir string) *Scanner {
	t.Helper()
	return New(Options{
		ProjectsDir:   projectsDir,
		CodewhaleDir:  codewhaleDir,
		IdleThreshold: 20 * time.Minute,
		MaxCards:      50,
	})
}

func TestScanIncludesCodewhaleSessions(t *testing.T) {
	projects := t.TempDir()
	store := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "repo", ".worktrees", "feat-x")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	const id = "11111111-2222-3333-4444-555555555555"
	writeCodewhaleLog(t, store, id,
		`{"type":"session_meta","payload":{"id":"`+id+`","cwd":"`+cwd+`"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do the cards"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"scanning the store"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","total":123}}`,
	)
	// Not a session: history/lock/state files share the directory.
	for _, junk := range []string{"composer_history.jsonl", ".reconcile.json", "update-check.json"} {
		if err := os.WriteFile(filepath.Join(store, junk), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sessions := newCodewhaleScanner(t, projects, store).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1: %+v", len(sessions), sessions)
	}
	got := sessions[0]
	if got.Source != session.SourceCodewhale {
		t.Errorf("Source = %q, want %q", got.Source, session.SourceCodewhale)
	}
	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	wantDir := EncodeForCursor(strings.TrimPrefix(cwd, "/"))
	if got.ProjectDir != wantDir {
		t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, wantDir)
	}
	if got.WorktreeSlug != "feat-x" {
		t.Errorf("WorktreeSlug = %q, want feat-x", got.WorktreeSlug)
	}
	if got.Project == "" {
		t.Error("Project label is empty")
	}
	// The newest record is a token-usage event with no role, so the
	// reader must walk back to the assistant message rather than give up.
	if got.LastRole != session.RoleAssistant {
		t.Errorf("LastRole = %q, want assistant", got.LastRole)
	}
	if got.LastText != "scanning the store" {
		t.Errorf("LastText = %q, want %q", got.LastText, "scanning the store")
	}
	if got.CWDHint != cwd {
		t.Errorf("CWDHint = %q, want %q", got.CWDHint, cwd)
	}
	if got.AgentLabel() != "codewhale" {
		t.Errorf("AgentLabel = %q, want codewhale", got.AgentLabel())
	}
	if got.ResumeCommand() != "codewhale resume "+id {
		t.Errorf("ResumeCommand = %q", got.ResumeCommand())
	}
}

func TestScanSkipsStaleCodewhaleSessions(t *testing.T) {
	projects := t.TempDir()
	store := t.TempDir()
	cwd := t.TempDir()

	old := writeCodewhaleLog(t, store, "aaaaaaaa-0000-0000-0000-000000000000",
		`{"type":"session_meta","payload":{"cwd":"`+cwd+`"}}`)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	writeCodewhaleLog(t, store, "bbbbbbbb-0000-0000-0000-000000000000",
		`{"type":"session_meta","payload":{"cwd":"`+cwd+`"}}`)

	sessions := newCodewhaleScanner(t, projects, store).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != "bbbbbbbb-0000-0000-0000-000000000000" {
		t.Fatalf("kept %q, want the fresh session", sessions[0].ID)
	}
}

// makeWorktreeRepo builds a git-shaped primary checkout with one
// worktree directory, mirroring what `n` produces on disk. It lives
// under /tmp with a hyphen-free name because project-dir decoding is
// ambiguous once a path contains `_` (the encoder collapses it to `-`),
// and t.TempDir() is not guaranteed to be free of them.
func makeWorktreeRepo(t *testing.T, slug string) (repo, worktree string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "amwt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	repo = filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(repo, ".worktrees", slug)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, worktree
}

// A worktree card must survive the idle cutoff: it is the only handle
// the dashboard has for resuming or removing that worktree, and the
// Cursor path already guarantees one card per worktree, always.
func TestCodewhaleWorktreeCardSurvivesIdleCutoff(t *testing.T) {
	projects := t.TempDir()
	store := t.TempDir()
	_, worktree := makeWorktreeRepo(t, "feat-cards")

	path := writeCodewhaleLog(t, store, "eeeeeeee-0000-0000-0000-000000000000",
		`{"type":"session_meta","payload":{"cwd":"`+worktree+`"}}`)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	sessions := newCodewhaleScanner(t, projects, store).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].WorktreeSlug != "feat-cards" {
		t.Fatalf("WorktreeSlug = %q, want feat-cards", sessions[0].WorktreeSlug)
	}
}

// The ghost-card guard applies to Codewhale too: once the worktree
// directory is gone, the idle cutoff takes the card with it.
func TestCodewhaleOrphanedWorktreeAgesOut(t *testing.T) {
	projects := t.TempDir()
	store := t.TempDir()
	_, worktree := makeWorktreeRepo(t, "feat-gone")
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}

	path := writeCodewhaleLog(t, store, "ffffffff-0000-0000-0000-000000000000",
		`{"type":"session_meta","payload":{"cwd":"`+worktree+`"}}`)
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	if sessions := newCodewhaleScanner(t, projects, store).Scan(); len(sessions) != 0 {
		t.Fatalf("orphaned worktree kept %d sessions, want 0", len(sessions))
	}
}

func TestScanCodewhaleMtimeCache(t *testing.T) {
	projects := t.TempDir()
	store := t.TempDir()
	cwd := t.TempDir()
	writeCodewhaleLog(t, store, "cccccccc-0000-0000-0000-000000000000",
		`{"type":"session_meta","payload":{"cwd":"`+cwd+`"}}`,
		`{"role":"assistant","content":"first"}`,
	)

	scn := newCodewhaleScanner(t, projects, store)
	first := scn.Scan()
	if len(first) != 1 || first[0].LastText != "first" {
		t.Fatalf("first scan = %+v", first)
	}
	// Same mtime → served from cache and still identical.
	second := scn.Scan()
	if len(second) != 1 || second[0].LastText != "first" {
		t.Fatalf("cached scan = %+v", second)
	}
}

func TestScanWithoutCursorInstall(t *testing.T) {
	store := t.TempDir()
	cwd := t.TempDir()
	writeCodewhaleLog(t, store, "dddddddd-0000-0000-0000-000000000000",
		`{"type":"session_meta","payload":{"cwd":"`+cwd+`"}}`,
	)
	// A machine with no Cursor install has no projects dir; Codewhale
	// sessions must still surface.
	missing := filepath.Join(t.TempDir(), "no-cursor")
	sessions := newCodewhaleScanner(t, missing, store).Scan()
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].Source != session.SourceCodewhale {
		t.Fatalf("Source = %q, want codewhale", sessions[0].Source)
	}
}

func TestCodewhaleCWDFallsBackToTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.json")
	// One record larger than the head window leaves only a partial line
	// there; the per-turn context in the tail must still supply the cwd.
	// The workspace has to be a real directory: extraction stats it.
	ws := t.TempDir()
	body := `{"type":"session_meta","payload":{"prompt":"` + strings.Repeat("x", codewhaleHeadBytes+1024) + `"}}` + "\n" +
		`{"type":"turn_context","payload":{"cwd":"` + ws + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := parseCodewhaleCWD(path); got != ws {
		t.Fatalf("cwd = %q, want %q", got, ws)
	}
}

// The workspace key is Codewhale's business; when it changes, the card
// must still find the directory from any absolute path in the log.
func TestCodewhaleCWDInfersFromExistingRepoPath(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := `{"type":"session_meta","payload":{"some_future_key":"` + repo + `","noise":"/does/not/exist"}}`
	if got := codewhaleCWD([]byte(log)); got != repo {
		t.Fatalf("cwd = %q, want %q", got, repo)
	}
}

func TestCodewhaleCWDRejectsNonexistentPath(t *testing.T) {
	log := `{"type":"session_meta","payload":{"noise":"/does/not/exist/anywhere"}}`
	if got := codewhaleCWD([]byte(log)); got != "" {
		t.Fatalf("cwd = %q, want empty", got)
	}
}

func TestCodewhaleCWDKeyTolerance(t *testing.T) {
	cwdA, cwdB, cwdC, cwdD := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	cases := []struct {
		name string
		log  string
		want string
	}{
		{"cwd", `{"type":"session_meta","payload":{"cwd":"` + cwdA + `"}}`, cwdA},
		{"working_directory", `{"type":"turn","working_directory":"` + cwdB + `"}`, cwdB},
		{"workspace", `{"workspace":"` + cwdC + `"}`, cwdC},
		{"nested", `{"payload":{"context":{"workspace_path":"` + cwdD + `"}}}`, cwdD},
		{"missing", `{"type":"session_meta","payload":{}}`, ""},
		{"relative-ignored", `{"payload":{"cwd":"repo"}}`, ""},
		// A key pointing at nothing on disk is not a workspace: stat
		// keeps a stale or tool-local path off the card.
		{"nonexistent-ignored", `{"payload":{"cwd":"/nope/not/a/dir"}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codewhaleCWD([]byte(c.log + "\n")); got != c.want {
				t.Fatalf("codewhaleCWD = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCodewhaleLastTurnToolCall(t *testing.T) {
	// A bare call record carries no role of its own; it is still the
	// assistant acting, so the card must read as ACTIVE, not IDLE.
	log := `{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}`
	role, tool, text := codewhaleLastTurn([]byte(log))
	if role != session.RoleAssistant || tool != "shell" || text != "" {
		t.Fatalf("call record = (%q, %q, %q), want (assistant, shell, \"\")", role, tool, text)
	}

	// A tool *result* is not a call: exact type matching must skip it.
	log = `{"type":"function_call_output","name":"shell","output":"ok"}`
	role, tool, text = codewhaleLastTurn([]byte(log))
	if role != session.RoleUnknown || tool != "" || text != "" {
		t.Fatalf("output record = (%q, %q, %q), want empty", role, tool, text)
	}

	log = `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"tool_use","name":"read_file"}]}}`
	role, tool, text = codewhaleLastTurn([]byte(log))
	if role != session.RoleAssistant {
		t.Fatalf("role = %q, want assistant", role)
	}
	if tool != "read_file" {
		t.Fatalf("tool = %q, want read_file", tool)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

// A stated workspace that merely contains the repo the session actually
// worked in is a launch directory, not a project: `workspace:
// ~/Documents` with every touched file under ~/Documents/repo must not
// put "Documents" on the card.
func TestResolveCWDPrefersRepoInsideStatedWorkspace(t *testing.T) {
	repo, _ := makeWorktreeRepo(t, "unused")
	file := filepath.Join(repo, "README.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(repo)
	head := `{"workspace":"` + parent + `"}` + "\n" +
		`{"type":"tool_use","input":{"path":"` + file + `"}}` + "\n"

	hit := resolveCWD([]byte(head), nil, "")
	if hit.path != repo {
		t.Fatalf("path = %q, want the checkout %q", hit.path, repo)
	}
	if hit.signal != "git checkout" {
		t.Fatalf("signal = %q, want git checkout", hit.signal)
	}
}

// When the stated workspace is itself a checkout, it wins outright — no
// heuristic gets to overrule a log that names the repo.
func TestResolveCWDMetadataKeyWinsWhenItIsARepo(t *testing.T) {
	repo, _ := makeWorktreeRepo(t, "unused")
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := `{"workspace":"` + repo + `"}` + "\n" +
		`{"path":"` + filepath.Join(other, "a.go") + `"}` + "\n"

	hit := resolveCWD([]byte(head), nil, "")
	if hit.path != repo {
		t.Fatalf("path = %q, want the stated workspace %q", hit.path, repo)
	}
	if hit.signal != "metadata key" {
		t.Fatalf("signal = %q, want metadata key", hit.signal)
	}
}

// A stated workspace with no repo anywhere near it is still the best
// answer available, and must be reported as such.
func TestResolveCWDKeepsPlainStatedWorkspace(t *testing.T) {
	ws := t.TempDir()
	head := `{"workspace":"` + ws + `"}`
	hit := resolveCWD([]byte(head), nil, "")
	if hit.path != ws || hit.signal != "metadata key" {
		t.Fatalf("got (%q, %q), want (%q, metadata key)", hit.path, hit.signal, ws)
	}
}

// Codewhale stores one JSON document per session, not JSONL. Reading it
// line-by-line returns the first message instead of the last turn — the
// bug that made an actively working session show as IDLE.
func TestParseCodewhaleDocLastTurn(t *testing.T) {
	doc := `{
	  "schema_version": 1,
	  "metadata": {"id": "abc", "workspace": "/Users/u/repo", "title": "t"},
	  "messages": [
	    {"role": "user", "content": "first prompt"},
	    {"role": "assistant", "content": [{"type": "text", "text": "first reply"}]},
	    {"role": "user", "content": [{"type": "tool_result", "content": "ok", "tool_use_id": "1"}]},
	    {"role": "assistant", "content": [{"type": "thinking", "thinking": "hmm"}, {"type": "tool_use", "name": "bash"}]}
	  ]
	}`

	// The stated workspace must exist for the parser to accept it.
	repo := t.TempDir()
	doc = strings.Replace(doc, "/Users/u/repo", repo, 1)

	role, tool, text, stated, ok := parseCodewhaleDoc([]byte(doc))
	if !ok {
		t.Fatal("document did not parse")
	}
	if role != session.RoleAssistant {
		t.Fatalf("role = %q, want assistant (the LAST message)", role)
	}
	if tool != "bash" {
		t.Fatalf("tool = %q, want bash", tool)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty (thinking is not reply text)", text)
	}
	if stated != repo {
		t.Fatalf("stated workspace = %q, want %q", stated, repo)
	}
}

func TestParseCodewhaleDocUserTurnAndEmpty(t *testing.T) {
	role, _, _, _, ok := parseCodewhaleDoc([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if !ok || role != session.RoleUser {
		t.Fatalf("got (%q, ok=%v), want user", role, ok)
	}

	role, _, _, _, ok = parseCodewhaleDoc([]byte(`{"metadata":{},"messages":[]}`))
	if !ok || role != session.RoleUnknown {
		t.Fatalf("empty messages: got (%q, ok=%v), want unknown", role, ok)
	}

	if _, _, _, _, ok := parseCodewhaleDoc([]byte(`not json`)); ok {
		t.Fatal("garbage parsed as a document")
	}
}

// End-to-end: a document-shaped log on disk must classify from its last
// turn, and state its workspace from metadata rather than by inference.
func TestParseCodewhaleTailReadsDocument(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()
	path := filepath.Join(dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json")
	doc := `{"metadata":{"workspace":"` + repo + `"},"messages":[` +
		`{"role":"user","content":"old prompt"},` +
		`{"role":"assistant","content":"old reply"},` +
		`{"role":"assistant","content":[{"type":"tool_use","name":"bash"}]}` +
		`]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	role, tool, text := parseCodewhaleTail(path, codewhaleTailFloor)
	if role != session.RoleAssistant || tool != "bash" || text != "" {
		t.Fatalf("got (%q, %q, %q), want (assistant, bash, \"\")", role, tool, text)
	}
	if got := parseCodewhaleCWD(path); got != repo {
		t.Fatalf("cwd = %q, want the stated workspace %q", got, repo)
	}
}

func TestEncodeCodewhaleCWD(t *testing.T) {
	if got := encodeCodewhaleCWD("/Users/u/Documents/repo/.worktrees/feat-x"); got != "Users-u-Documents-repo-worktrees-feat-x" {
		t.Fatalf("encodeCodewhaleCWD = %q", got)
	}
	if got := encodeCodewhaleCWD(""); got != "" {
		t.Fatalf("encodeCodewhaleCWD(\"\") = %q, want empty", got)
	}
}
