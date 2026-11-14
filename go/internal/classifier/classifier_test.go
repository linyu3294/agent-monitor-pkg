package classifier

import (
	"testing"
	"time"

	"github.com/agent-monitor/agent-monitor/internal/session"
)

type stubView struct {
	live, paused map[string]bool
}

func (s stubView) IsLive(id string) bool   { return s.live[id] }
func (s stubView) IsPaused(id string) bool { return s.paused[id] }

func TestClassify(t *testing.T) {
	now := time.Now().Unix()
	live := stubView{live: map[string]bool{"a": true}}
	dead := stubView{}
	paused := stubView{live: map[string]bool{"a": true}, paused: map[string]bool{"a": true}}

	cases := []struct {
		name string
		sess session.Session
		view View
		want session.State
	}{
		{
			name: "dead process is idle even with fresh mtime",
			sess: session.Session{ID: "a", Mtime: now, LastRole: session.RoleAssistant},
			view: dead,
			want: session.StateIdle,
		},
		{
			name: "paused trumps role",
			sess: session.Session{ID: "a", Mtime: now, LastRole: session.RoleAssistant},
			view: paused,
			want: session.StatePaused,
		},
		{
			name: "stale (>2min) is idle",
			sess: session.Session{ID: "a", Mtime: now - 600, LastRole: session.RoleAssistant, LastTool: "Shell"},
			view: live,
			want: session.StateIdle,
		},
		{
			name: "fresh write within 5s is active",
			sess: session.Session{ID: "a", Mtime: now - 1, LastRole: session.RoleAssistant},
			view: live,
			want: session.StateActive,
		},
		{
			name: "assistant + text only is idle (the THINKING-stuck bug)",
			sess: session.Session{ID: "a", Mtime: now - 60, LastRole: session.RoleAssistant, LastText: "Hi - what's up?"},
			view: live,
			want: session.StateIdle,
		},
		{
			name: "assistant + tool_use is active",
			sess: session.Session{ID: "a", Mtime: now - 60, LastRole: session.RoleAssistant, LastTool: "Shell"},
			view: live,
			want: session.StateActive,
		},
		{
			name: "user role is thinking (model generating)",
			sess: session.Session{ID: "a", Mtime: now - 30, LastRole: session.RoleUser},
			view: live,
			want: session.StateThinking,
		},
		{
			name: "unknown role falls back to time window",
			sess: session.Session{ID: "a", Mtime: now - 20, LastRole: session.RoleUnknown},
			view: live,
			want: session.StateActive,
		},
		// A Codewhale log is rewritten per turn, so a multi-minute tool
		// call leaves mtime stale while the agent is working. Its
		// liveness is an open live-marker fd, which a dead session
		// cannot hold, so the clock must not force IDLE.
		{
			name: "stale codewhale with a live fd and a tool in flight is active",
			sess: session.Session{ID: "a", Source: session.SourceCodewhale, Mtime: now - 600, LastRole: session.RoleAssistant, LastTool: "bash"},
			view: live,
			want: session.StateActive,
		},
		{
			name: "stale codewhale whose turn ended is idle",
			sess: session.Session{ID: "a", Source: session.SourceCodewhale, Mtime: now - 600, LastRole: session.RoleAssistant, LastText: "done"},
			view: live,
			want: session.StateIdle,
		},
		{
			name: "stale codewhale with no live fd is still idle",
			sess: session.Session{ID: "a", Source: session.SourceCodewhale, Mtime: now - 600, LastRole: session.RoleAssistant, LastTool: "bash"},
			view: dead,
			want: session.StateIdle,
		},
		// Cursor keeps the clock rule: its pids idle without recording.
		{
			name: "stale cursor stays idle despite a live pid",
			sess: session.Session{ID: "a", Source: session.SourceCursor, Mtime: now - 600, LastRole: session.RoleAssistant, LastTool: "Shell"},
			view: live,
			want: session.StateIdle,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.sess, c.view)
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
