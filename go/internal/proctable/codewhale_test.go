package proctable

import (
	"strconv"
	"strings"
	"testing"
)

func TestLooksLikeCodewhale(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"/opt/homebrew/bin/codewhale", true},
		{"/opt/homebrew/bin/codewhale --approval-policy never --sandbox-mode workspace-write", true},
		{"codewhale resume 11111111-2222-3333-4444-555555555555", true},
		{"/usr/local/bin/codewhale-helper", false},
		{"grep codewhale /tmp/log", false},
		{"bash -c 'echo codewhale'", false},
		{"/Users/u/.cursor-agent/versions/1/agent --use-system-ca", false},
	}
	for _, c := range cases {
		if got := looksLikeCodewhale([]byte(c.cmd)); got != c.want {
			t.Errorf("looksLikeCodewhale(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestCodewhaleIDFromOpenPath(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	store := "/Users/u/.codewhale/sessions"
	cases := []struct {
		name string
		path string
		want string
	}{
		// The shape observed on 0.10.x: a live marker kept open for the
		// duration of the session (see `lsof -p <codewhale pid>`).
		{"live marker", store + "/.late-usage/" + id + ".live", id},
		{"live marker, cloned store", "/srv/o/.codewhale/sessions/.late-usage/" + id + ".live", id},
		{"session log", store + "/" + id + ".json", id},
		// A `.live` file outlives its process, so a lock file or marker
		// seen on disk is not evidence; only lsof output reaches here.
		// Everything below must be rejected even when lsof reports it.
		{"lock beside marker", store + "/.late-usage/" + id + ".lock", ""},
		{"checkpoint queue", store + "/checkpoints/" + id + ".offline_queue.lock", ""},
		{"runtime owner lock", store + "/" + id + "/runtime/runtime-process.owner.lock", ""},
		{"owner json", store + "/" + id + "/runtime/owner.json", ""},
		{"temp file", store + "/.tmpHRdPCT", ""},
		{"non-uuid", store + "/session_boot_owners.json", ""},
		{"deep", store + "/.late-usage/nested/" + id + ".live", ""},
		{"unrelated", "/Users/u/.cursor/chats/h/" + id + "/store.db", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codewhaleIDFromOpenPath(c.path, store); got != c.want {
				t.Fatalf("codewhaleIDFromOpenPath(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

// lsofLine builds one default-format lsof row: 9 whitespace fields with
// the open file last, which is the shape bindFromLsof parses.
func lsofLine(cmd string, pid int, name string) string {
	return cmd + " " + strconv.Itoa(pid) + " user 3w REG 1,2 12345 678 " + name
}

func TestBindFromLsofCodewhale(t *testing.T) {
	const codewhaleID = "8512a2ac-fa7b-4668-9f2a-58ed5e0c99c6"
	const cursorID = "afd3a58d-8fb9-40ad-b3da-c88da1e575dc"
	store := "/Users/u/.codewhale/sessions"

	out := strings.Join([]string{
		"COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME",
		lsofLine("codewhale", 71383, store+"/.late-usage/"+codewhaleID+".live"),
		lsofLine("codewhale", 71383, store+"/.late-usage/"+codewhaleID+".lock"),
		lsofLine("codewhale", 71383, store+"/.tmpHRdPCT"),
		lsofLine("cursor-agent", 4242, "/Users/u/.cursor/chats/abc123/"+cursorID+"/store.db"),
		// A codewhale pid holding only temp files must not bind.
		lsofLine("codewhale", 99999, store+"/.tmpOTHER"),
	}, "\n")

	tbl := &Table{byChat: map[string]Process{}, codewhaleDir: store}
	tbl.bindFromLsof([]byte(out), map[int]agentProc{
		71383: {pid: 71383, state: 'S', codewhale: true},
		4242:  {pid: 4242, state: 'R'},
		99999: {pid: 99999, state: 'S', codewhale: true},
	})

	if !tbl.IsLive(codewhaleID) {
		t.Fatal("codewhale session not bound from its open live marker")
	}
	if got := tbl.PID(codewhaleID); got != 71383 {
		t.Fatalf("PID = %d, want 71383", got)
	}
	// Cursor's store.db path mapping must be untouched by the rework.
	if !tbl.IsLive(cursorID) {
		t.Fatal("cursor chat not bound from its open store.db")
	}
	if got := tbl.PID(cursorID); got != 4242 {
		t.Fatalf("cursor PID = %d, want 4242", got)
	}
	if tbl.IsLive(".tmpHRdPCT") || tbl.IsLive(".tmpOTHER") {
		t.Fatal("temp files were bound as sessions")
	}
}

// bindCodewhaleViaLsof is the ps-free fallback: it must parse lsof's
// field-mode output (-Fpcn) and bind the live marker it sees there.
func TestBindCodewhaleViaLsofFieldOutput(t *testing.T) {
	const id = "85078aaa-1111-2222-3333-444444444444"
	store := "/Users/u/.codewhale/sessions"
	// Real -Fpcn shape: p/c lines per process, then one n line per open
	// file. Repeated p lines are normal (one per file group).
	out := strings.Join([]string{
		"p85078",
		"ccodewhale",
		"n" + store + "/.late-usage/" + id + ".live",
		"n" + store + "/.late-usage/" + id + ".lock",
		"n/opt/homebrew/bin/codewhale",
		"p99999",
		"ccodewhale-helper",
		"n" + store + "/.late-usage/99999999-1111-2222-3333-444444444444.live",
	}, "\n")

	// Parse the same way bindCodewhaleViaLsof does, over the captured
	// output, without spawning lsof.
	tbl := &Table{byChat: map[string]Process{}, codewhaleDir: store}
	tbl.bindCodewhaleFromFields([]byte(out))

	if !tbl.IsLive(id) {
		t.Fatal("live marker in -Fpcn output was not bound")
	}
	if got := tbl.PID(id); got != 85078 {
		t.Fatalf("PID = %d, want 85078", got)
	}
	if len(tbl.byChat) != 1 {
		t.Fatalf("bound %d sessions, want exactly 1: %v", len(tbl.byChat), tbl.byChat)
	}
}

func TestRegistryPairs(t *testing.T) {
	const a = "aaaaaaaa-0000-0000-0000-000000000000"
	const b = "bbbbbbbb-0000-0000-0000-000000000000"
	const c = "cccccccc-0000-0000-0000-000000000000"

	cases := []struct {
		name string
		data string
		want map[string]int
	}{
		{
			name: "id-to-pid map",
			data: `{"` + a + `":42117}`,
			want: map[string]int{a: 42117},
		},
		{
			name: "records",
			data: `{"sessions":[{"id":"` + b + `","pid":99}]}`,
			want: map[string]int{b: 99},
		},
		{
			name: "jsonl",
			data: "{\"session_id\":\"" + c + "\",\"owner_pid\":1234}\n",
			want: map[string]int{c: 1234},
		},
		{
			name: "unpaired numbers ignored",
			data: `{"pid":42117,"started_at":1763000000}`,
			want: map[string]int{},
		},
		{
			name: "not json",
			data: `42117`,
			want: map[string]int{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := registryPairs([]byte(tc.data))
			if len(got) != len(tc.want) {
				t.Fatalf("registryPairs = %v, want %v", got, tc.want)
			}
			for id, pid := range tc.want {
				if got[id] != pid {
					t.Fatalf("registryPairs[%s] = %d, want %d", id, got[id], pid)
				}
			}
		})
	}
}

func TestOwnerMatch(t *testing.T) {
	live := map[int]byte{42117: 'S', 42118: 'T'}

	if pid, state, ok := ownerMatch([]byte(`{"pid":42117}`), live); !ok || pid != 42117 || state != 'S' {
		t.Fatalf("ownerMatch = (%d, %q, %v), want (42117, S, true)", pid, state, ok)
	}
	// A stale owner record names a pid this tick never saw as codewhale.
	if _, _, ok := ownerMatch([]byte(`{"pid":999}`), live); ok {
		t.Fatal("stale owner record matched a dead pid")
	}
	if _, _, ok := ownerMatch([]byte(`not json`), live); ok {
		t.Fatal("garbage owner record matched")
	}
}

func TestCollectPIDs(t *testing.T) {
	cases := []struct {
		name string
		data string
		want []int
	}{
		{"pid", `{"pid":42117,"started_at":1763000000}`, []int{42117}},
		{"nested", `{"owner":{"pid":99}}`, []int{99}},
		{"jsonl", "{\"pid\":7}\n{\"pid\":8}\n", []int{7, 8}},
		{"rejects-epochs", `{"started_at":1763000000}`, nil},
		{"not-json", "42117", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collectPIDs([]byte(c.data))
			if len(got) != len(c.want) {
				t.Fatalf("collectPIDs = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("collectPIDs = %v, want %v", got, c.want)
				}
			}
		})
	}
}
