// Codewhale liveness.
//
// A Cursor chat is mapped to its process through an open store.db, which
// yields the chat id directly. Codewhale sessions are mapped the same
// way where possible, and through two fallbacks where not:
//
//  1. `lsof` on every live codewhale pid. A running session keeps its
//     log open at `~/.codewhale/sessions/<uuid>.json`, so the open file
//     is the session id — no schema knowledge required. This is the same
//     open-file mechanism the Cursor path uses, and it rides along in
//     the bulk lsof call that already runs each tick.
//  2. The store's ownership records, for a runtime that writes its log
//     with open/append/close and therefore holds no fd: the shared
//     `session_boot_owners.json` and the per-session
//     `<uuid>/runtime/owner.json`.
//
// Every candidate is validated against this tick's `ps` snapshot, so a
// stale record naming a dead pid cannot resurrect a card as "live".
package proctable

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// maxPID bounds the integers we'll treat as process ids. macOS tops out
// at 99998 by default; 10M leaves room for tuned kernels while still
// rejecting epoch timestamps and counters.
const maxPID = 10_000_000

// codewhaleSessionDir is the path segment that identifies a Codewhale
// session log among a process's open files.
const codewhaleSessionDir = "/.codewhale/sessions/"

var codewhaleIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// defaultCodewhaleDir resolves the Codewhale session store.
func defaultCodewhaleDir() string {
	if dir := os.Getenv("AM_CODEWHALE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codewhale", "sessions")
}

// looksLikeCodewhale matches only a process whose argv[0] basename is
// exactly "codewhale". Substring matching would light up cards for any
// process that merely mentions the word (a grep, an editor buffer, a
// shell command).
func looksLikeCodewhale(cmd []byte) bool {
	first := cmd
	if i := bytes.IndexByte(first, ' '); i >= 0 {
		first = first[:i]
	}
	if i := bytes.LastIndexByte(first, '/'); i >= 0 {
		first = first[i+1:]
	}
	return bytes.Equal(first, []byte("codewhale"))
}

// codewhaleIDFromOpenPath extracts the session id from an open file that
// proves a session is live. Codewhale holds one of two shapes while
// running:
//
//	<store>/.late-usage/<uuid>.live     (observed on 0.10.x)
//	<store>/<uuid>.json                 (if the log itself stays open)
//
// Everything else in the store — lock files, checkpoint queues, runtime
// owner files — is deliberately rejected, because a leftover `.live`
// file from a dead session is indistinguishable from a live one by name
// alone. Only an open file descriptor proves liveness, so this function
// is called with paths reported by lsof.
func codewhaleIDFromOpenPath(name, sessionDir string) string {
	prefix := strings.TrimSuffix(sessionDir, "/") + "/"
	var rest string
	switch {
	case sessionDir != "" && strings.HasPrefix(name, prefix):
		rest = name[len(prefix):]
	case strings.Contains(name, codewhaleSessionDir):
		// Store cloned or symlinked elsewhere: fall back to the marker.
		_, rest, _ = strings.Cut(name, codewhaleSessionDir)
	default:
		return ""
	}

	parts := strings.Split(rest, "/")
	if len(parts) == 0 || len(parts) > 2 {
		return ""
	}
	base := parts[len(parts)-1]
	dir := ""
	if len(parts) == 2 {
		dir = parts[0]
	}

	var id string
	switch {
	case dir == "" && strings.HasSuffix(base, ".json"):
		id = strings.TrimSuffix(base, ".json")
	case dir == lateUsageDir && strings.HasSuffix(base, ".live"):
		id = strings.TrimSuffix(base, ".live")
	default:
		return ""
	}
	if !codewhaleIDRe.MatchString(id) {
		return ""
	}
	return id
}

// lateUsageDir is the subdirectory holding the per-session live markers
// Codewhale opens (and keeps open) for the duration of a session.
const lateUsageDir = ".late-usage"

// bindCodewhaleViaLsof enumerates codewhale processes by command name and
// binds session ids from the live markers they hold open. Used as the
// fallback when `ps` is unavailable or restricted, which is common inside
// sandboxes: lsof needs no process-table access.
func (t *Table) bindCodewhaleViaLsof() {
	out, err := exec.Command("lsof", "-c", "codewhale", "-Fpcn").Output()
	if err != nil && len(out) == 0 {
		return
	}
	t.bindCodewhaleFromFields(out)
}

// bindCodewhaleFromFields is the pure half of the fallback, so the
// field-mode parser is testable against captured lsof output.
func (t *Table) bindCodewhaleFromFields(out []byte) {
	pid := 0
	isCodewhale := false
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			isCodewhale = false
		case 'c':
			// Exact name only. `lsof -c codewhale` also matches
			// `codewhale-helper`, and a helper that inherited the live
			// marker's fd would keep a dead session looking alive.
			isCodewhale = strings.TrimSpace(line[1:]) == "codewhale"
		case 'n':
			if !isCodewhale || pid == 0 {
				continue
			}
			if id := codewhaleIDFromOpenPath(line[1:], t.codewhaleDir); id != "" {
				// State is unknown without ps; the zero value would read
				// as neither running nor stopped, so use the neutral
				// sleeping letter.
				t.byChat[id] = Process{PID: pid, State: 'S'}
			}
		}
	}
}

// bindCodewhaleRegistry reads the store's ownership records and binds
// any session whose recorded pid this tick also saw as a codewhale
// process. Everything is best-effort: a missing or malformed record
// simply leaves the card without a live process.
func (t *Table) bindCodewhaleRegistry(live map[int]byte) {
	if len(live) == 0 || t.codewhaleDir == "" {
		return
	}
	if data, err := os.ReadFile(filepath.Join(t.codewhaleDir, "session_boot_owners.json")); err == nil {
		for id, pid := range registryPairs(data) {
			if state, ok := live[pid]; ok {
				if _, taken := t.byChat[id]; !taken {
					t.byChat[id] = Process{PID: pid, State: state}
				}
			}
		}
	}
	entries, err := os.ReadDir(t.codewhaleDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if _, taken := t.byChat[id]; taken || !codewhaleIDRe.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(t.codewhaleDir, id, "runtime", "owner.json"))
		if err != nil {
			continue
		}
		if pid, state, ok := ownerMatch(data, live); ok {
			t.byChat[id] = Process{PID: pid, State: state}
		}
	}
}

// registryPairs pulls session-id → pid associations out of an ownership
// file without pinning its schema. Handles the two shapes a registry
// takes in practice — `{"<uuid>": <pid>}` and objects holding both
// values — and ignores everything else.
//
// This is a tertiary signal: the primary proof of liveness is an open
// live marker (see bindCodewhaleViaLsof), because a registry entry
// outlives the process it names.
func registryPairs(data []byte) map[string]int {
	out := map[string]int{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || (line[0] != '{' && line[0] != '[') {
			continue
		}
		var v any
		if err := json.Unmarshal(line, &v); err != nil {
			continue
		}
		collectRegistryPairs(v, out)
	}
	return out
}

func collectRegistryPairs(v any, out map[string]int) {
	switch t := v.(type) {
	case map[string]any:
		var ids []string
		var pids []int
		for k, child := range t {
			// `{"<uuid>": <pid>}` and `{"<pid>": "<uuid>"}` map forms.
			if codewhaleIDRe.MatchString(k) {
				if pid, ok := asPID(child); ok {
					out[k] = pid
					continue
				}
			}
			if pid, ok := asPID(k); ok {
				if s, ok := child.(string); ok && codewhaleIDRe.MatchString(s) {
					out[s] = pid
					continue
				}
			}
			if s, ok := child.(string); ok && codewhaleIDRe.MatchString(s) {
				ids = append(ids, s)
			}
			if pid, ok := asPID(child); ok {
				pids = append(pids, pid)
			}
		}
		// `{"id": "<uuid>", "pid": 1234}` — one of each is unambiguous.
		if len(ids) == 1 && len(pids) == 1 {
			if _, exists := out[ids[0]]; !exists {
				out[ids[0]] = pids[0]
			}
		}
		for _, child := range t {
			collectRegistryPairs(child, out)
		}
	case []any:
		for _, child := range t {
			collectRegistryPairs(child, out)
		}
	}
}

// asPID reports a plausible process id from either a JSON number or a
// numeric string.
func asPID(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t >= 1 && t <= maxPID && t == math.Trunc(t) {
			return int(t), true
		}
	case string:
		n, err := strconv.Atoi(t)
		if err == nil && n >= 1 && n <= maxPID {
			return n, true
		}
	}
	return 0, false
}

// ownerMatch returns the pid named by a per-session ownership record
// that this tick's snapshot also saw as a Codewhale process.
func ownerMatch(data []byte, live map[int]byte) (int, byte, bool) {
	for _, pid := range collectPIDs(data) {
		if state, ok := live[pid]; ok {
			return pid, state, true
		}
	}
	return 0, 0, false
}

// collectPIDs gathers every plausible pid-shaped integer in a JSON or
// JSONL record. Key names are deliberately not pinned: Codewhale owns
// this file's schema, and a matched pid is proof enough.
func collectPIDs(data []byte) []int {
	var out []int
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || (line[0] != '{' && line[0] != '[') {
			continue
		}
		var v any
		if err := json.Unmarshal(line, &v); err != nil {
			continue
		}
		walkNumbers(v, &out)
	}
	return out
}

func walkNumbers(v any, out *[]int) {
	switch t := v.(type) {
	case float64:
		if t >= 1 && t <= maxPID && t == math.Trunc(t) {
			*out = append(*out, int(t))
		}
	case map[string]any:
		for _, child := range t {
			walkNumbers(child, out)
		}
	case []any:
		for _, child := range t {
			walkNumbers(child, out)
		}
	}
}
