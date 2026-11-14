package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

// writeCursorTranscript lays out a transcript the way Cursor does for a
// worktree-rooted session:
//
//	<projects>/<encoded-primary>-worktrees-<encoded-slug>/agent-transcripts/<id>/<id>.jsonl
func writeCursorTranscript(t *testing.T, projects, primary, slug, id string) string {
	t.Helper()
	encoded := EncodeForCursor(trimLeadingSlash(primary)) + "-worktrees-" + EncodeForCursor(slug)
	dir := filepath.Join(projects, encoded, "agent-transcripts", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	body := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func trimLeadingSlash(s string) string {
	if len(s) > 0 && s[0] == '/' {
		return s[1:]
	}
	return s
}

// Removal deletes the worktree directory but keeps the session log, and
// card tombstones live only in memory. Both agent types must still be
// able to identify the card as worktree-backed after that, so the
// dashboard can recognise it as orphaned instead of relabelling it as a
// primary session on the parent repo.
func TestRemovedWorktreeCardKeepsItsIdentity(t *testing.T) {
	const id = "aaaaaaaa-1111-2222-3333-444444444444"

	cases := []struct {
		name  string
		setup func(t *testing.T, projects, store string) string
	}{
		{
			name: "codewhale",
			setup: func(t *testing.T, projects, store string) string {
				_, worktree := makeWorktreeRepo(t, "feat-removed")
				writeCodewhaleLog(t, store, id,
					`{"metadata":{"workspace":"`+worktree+`"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"bash"}]}]}`,
				)
				return worktree
			},
		},
		{
			name: "cursor",
			setup: func(t *testing.T, projects, store string) string {
				repo, worktree := makeWorktreeRepo(t, "feat-removed")
				writeCursorTranscript(t, projects, repo, "feat-removed", id)
				return worktree
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projects := t.TempDir()
			store := t.TempDir()
			worktree := tc.setup(t, projects, store)

			// Remove the worktree the way `r` does; leave the log alone.
			if err := os.RemoveAll(worktree); err != nil {
				t.Fatal(err)
			}

			sessions := newCodewhaleScanner(t, projects, store).Scan()
			if len(sessions) != 1 {
				t.Fatalf("Scan returned %d sessions, want 1", len(sessions))
			}
			got := sessions[0]
			if got.WorktreeSlug == "" {
				t.Fatalf("worktree identity lost: %+v", got)
			}
			if !IsOrphanedWorktree(got.ProjectDir) {
				t.Fatalf("card not recognised as orphaned: projectDir=%q", got.ProjectDir)
			}
		})
	}
}

// The counterpart: while the worktree exists, the card is not orphaned —
// a live worktree must never be hidden.
func TestExistingWorktreeCardIsNotOrphaned(t *testing.T) {
	projects := t.TempDir()
	store := t.TempDir()
	repo, worktree := makeWorktreeRepo(t, "feat-alive")
	writeCursorTranscript(t, projects, repo, "feat-alive", "bbbbbbbb-1111-2222-3333-444444444444")
	writeCodewhaleLog(t, store, "cccccccc-1111-2222-3333-444444444444",
		`{"metadata":{"workspace":"`+worktree+`"},"messages":[{"role":"assistant","content":"hi"}]}`,
	)

	sessions := newCodewhaleScanner(t, projects, store).Scan()
	if len(sessions) != 2 {
		t.Fatalf("Scan returned %d sessions, want 2", len(sessions))
	}
	for _, s := range sessions {
		if s.WorktreeSlug == "" {
			t.Fatalf("worktree slug missing on %+v", s)
		}
		if IsOrphanedWorktree(s.ProjectDir) {
			t.Fatalf("existing worktree reported orphaned: %+v", s)
		}
	}
}

// A stated workspace that is simply gone (not under .worktrees/) is still
// rejected, so a stale tool argument cannot become the card's project.
func TestStatedWorkspaceStillRequiresExistence(t *testing.T) {
	head := `{"metadata":{"workspace":"/no/such/dir/anywhere"}}`
	if got := resolveCWD([]byte(head), nil, "").path; got != "" {
		t.Fatalf("cwd = %q, want empty", got)
	}
	// But a removed worktree path is kept, so orphan detection can work.
	head = `{"metadata":{"workspace":"/no/such/dir/.worktrees/feat-x"}}`
	if got := resolveCWD([]byte(head), nil, "").path; got != "/no/such/dir/.worktrees/feat-x" {
		t.Fatalf("cwd = %q, want the stated worktree path", got)
	}
}

var _ = session.SourceCursor
