// Package proctable resolves session ids to user-facing agent
// processes. The mapping is built once per refresh tick by
//
//  1. Listing all processes with `ps -Awwo pid,state,command`.
//  2. Keeping pids whose command line looks like a cursor-agent,
//     codewhale, or claude process.
//  3. For each candidate pid, looking up open files with `lsof` and
//     extracting the session id: Cursor through
//     `~/.cursor/chats/<projectHash>/<chatId>/store.db` (which the
//     user-facing cursor-agent keeps open for-write while the chat is
//     live), Codewhale through its open session log or live marker.
//
// The lookup in step 3 is done with a single bulk `lsof -p p1,p2,…`
// invocation rather than per-pid, which is the main speedup over the
// old shell implementation.
//
// Why store.db, not the per-project worker-server: the worker-server is
// a singleton per workspace, so signaling it would affect every sibling
// chat. The user-facing process is per-chat, so per-chat pause / resume
// / kill works cleanly.
//
// Codewhale sessions have no equivalent open file; they are bound
// through their runtime ownership record instead. See codewhale.go.
// Claude Code holds nothing open at all, so it is bound through the
// process's working directory; see claude.go.
package proctable

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Process represents one live user-facing agent process (Cursor,
// Codewhale, or Claude Code).
type Process struct {
	PID       int
	State     byte // ps stat letter: 'R' running, 'S' sleeping, 'T' stopped
	WtManager bool // true if argv contains "agent-monitor wt-manager"
}

// Table maps chat id (UUID string) → Process.
type Table struct {
	byChat       map[string]Process
	codewhaleDir string
	claudeDir    string
	// claudeProcs are live Claude Code processes with the directory each
	// runs in. Claude Code holds no session file descriptor, so its
	// process binds to a session by workspace instead of by open file;
	// the join happens in BindClaudeSessions once the scan knows which
	// session belongs to which directory.
	claudeProcs []claudeProc
}

// New returns an empty table. Call Refresh before reading.
func New() *Table {
	return &Table{
		byChat:       make(map[string]Process),
		codewhaleDir: defaultCodewhaleDir(),
		claudeDir:    defaultClaudeDir(),
	}
}

// agentProc is one live agent process found in this tick's ps snapshot.
type agentProc struct {
	pid       int
	state     byte
	started   time.Time
	wtManager bool
	codewhale bool
	claude    bool
}

// parseElapsed converts macOS ps `etime` output into a duration. The
// observed shapes are "MM:SS", "HH:MM:SS", and "D-HH:MM:SS"; an
// unparseable value yields 0, which the caller treats as "start time
// unknown" rather than inventing one.
func parseElapsed(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0
		}
		days = n
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var nums [3]int
	offset := len(nums) - len(parts)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0
		}
		nums[offset+i] = n
	}
	return time.Duration(days)*24*time.Hour +
		time.Duration(nums[0])*time.Hour +
		time.Duration(nums[1])*time.Minute +
		time.Duration(nums[2])*time.Second
}

// cutField splits the leading whitespace-delimited field off line and
// returns it with the remainder trimmed of leading spaces.
func cutField(line []byte) (field, rest []byte, ok bool) {
	line = bytes.TrimLeft(line, " \t")
	if len(line) == 0 {
		return nil, nil, false
	}
	if i := bytes.IndexByte(line, ' '); i >= 0 {
		return line[:i], bytes.TrimLeft(line[i+1:], " "), true
	}
	if i := bytes.IndexByte(line, '\t'); i >= 0 {
		return line[:i], bytes.TrimLeft(line[i+1:], " \t"), true
	}
	return line, nil, true
}

// Refresh rebuilds the chat → process mapping. Cheap enough to call once
// per tick (~25ms on a busy machine).
func (t *Table) Refresh() {
	t.byChat = map[string]Process{}
	t.claudeProcs = nil

	// Step 1: ps. We want pid, state, elapsed time, command. macOS ps
	// `state` is the canonical single-letter status (R/S/T/Z/...);
	// `etime` is the process's age, which Claude Code's liveness join
	// needs to tell a live session from a finished one in the same
	// directory (see BindClaudeSessions).
	snapshotAt := time.Now()
	psOut, err := exec.Command("ps", "-Awwo", "pid=,state=,etime=,command=").Output()
	if err != nil {
		// Restricted or sandboxed environments can deny ps while still
		// allowing lsof. Codewhale sessions remain discoverable without
		// the process table; Cursor chats need ps for their pid list, so
		// they degrade to "not live" rather than to a wrong answer.
		t.bindCodewhaleViaLsof()
		return
	}

	var procs []agentProc
	liveCodewhale := map[int]byte{}

	for _, raw := range bytes.Split(psOut, []byte{'\n'}) {
		line := bytes.TrimLeft(raw, " \t")
		if len(line) == 0 {
			continue
		}
		// "<pid> <state> <etime> <cmdline>". The command is the rest of
		// the line because it contains spaces; the three fields before it
		// do not, so a token walk is exact.
		pidStr, rest, ok := cutField(line)
		if !ok {
			continue
		}
		stateStr, rest, ok := cutField(rest)
		if !ok || len(stateStr) == 0 {
			continue
		}
		// state can be e.g. "S+", we just want the first letter.
		state := stateStr[0]
		etime, cmd, ok := cutField(rest)
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(string(pidStr))
		if err != nil {
			continue
		}
		started := snapshotAt.Add(-parseElapsed(string(etime)))
		if looksLikeCodewhale(cmd) {
			liveCodewhale[pid] = state
			procs = append(procs, agentProc{pid: pid, state: state, started: started, codewhale: true})
			continue
		}
		if looksLikeClaude(cmd) {
			procs = append(procs, agentProc{pid: pid, state: state, started: started, claude: true})
			continue
		}
		if !looksLikeCursorAgent(cmd) {
			continue
		}
		isWtm := bytes.Contains(cmd, []byte("agent-monitor wt-manager"))
		procs = append(procs, agentProc{pid: pid, state: state, started: started, wtManager: isWtm})
	}

	if len(procs) == 0 {
		// ps worked and found no agent processes at all. The registry
		// is still consulted because it is a cheap stat-only read, but
		// the lsof sweep is skipped: it can only rediscover the same
		// argv shapes ps already rejected, and this branch is the
		// steady state on an idle machine.
		t.bindCodewhaleRegistry(liveCodewhale)
		return
	}

	// Step 2: bulk lsof. One invocation for every live agent process of
	// either family; the open file is what identifies the session.
	pidArgs := make([]string, 0, len(procs))
	pidIndex := make(map[int]agentProc, len(procs))
	for _, p := range procs {
		pidArgs = append(pidArgs, strconv.Itoa(p.pid))
		pidIndex[p.pid] = p
	}
	lsofOut, err := exec.Command("lsof", "-p", strings.Join(pidArgs, ",")).Output()
	if err != nil {
		// lsof exits non-zero if any of the pids exited between the
		// `ps` call and now; the partial stdout is still usable.
		if _, ok := err.(*exec.ExitError); !ok {
			return
		}
	}

	t.bindFromLsof(lsofOut, pidIndex)

	// Registry fallback: covers a runtime that writes its log with
	// open/append/close and therefore holds no fd. Runs after lsof so
	// the open-file signal always wins.
	t.bindCodewhaleRegistry(liveCodewhale)
}

// bindFromLsof maps open files back to sessions for both agent families.
// Pure over its inputs so the parser is testable without live processes.
func (t *Table) bindFromLsof(lsofOut []byte, pidIndex map[int]agentProc) {
	for _, raw := range bytes.Split(lsofOut, []byte{'\n'}) {
		if len(raw) == 0 || raw[0] == 'C' /* header */ {
			continue
		}
		fields := bytes.Fields(raw)
		if len(fields) < 9 {
			continue
		}
		pid, err := strconv.Atoi(string(fields[1]))
		if err != nil {
			continue
		}
		proc, ok := pidIndex[pid]
		if !ok {
			continue
		}
		name := string(fields[len(fields)-1])

		if proc.claude {
			// Claude Code normally holds no session file. When it does hold
			// one open, that path names the session exactly, so bind it and
			// mark the process so the workspace join leaves it alone.
			if id := t.claudeID(name); id != "" {
				if _, taken := t.byChat[id]; !taken {
					t.byChat[id] = Process{PID: pid, State: proc.state}
					t.markClaudeExact(pid, proc.state, proc.started)
				}
				continue
			}
			// Otherwise the durable signal is the process's own working
			// directory. lsof reports it as the `cwd` descriptor of every
			// process it is given, which rides along in this same call.
			if bytes.Equal(fields[3], []byte("cwd")) {
				t.setClaudeCwd(pid, proc.state, proc.started, name)
			}
			continue
		}

		if proc.codewhale {
			if id := codewhaleIDFromOpenPath(name, t.codewhaleDir); id != "" {
				t.byChat[id] = Process{PID: pid, State: proc.state}
			}
			continue
		}

		// Cursor: .../.cursor/chats/<projectHash>/<chatId>/store.db
		if !strings.HasSuffix(name, "/store.db") {
			continue
		}
		i := strings.Index(name, "/.cursor/chats/")
		if i < 0 {
			continue
		}
		// Walk parts after `chats/`: <projectHash>/<chatId>/store.db
		after := name[i+len("/.cursor/chats/"):]
		first := strings.IndexByte(after, '/')
		if first < 0 {
			continue
		}
		rest := after[first+1:]
		second := strings.IndexByte(rest, '/')
		if second < 0 {
			continue
		}
		chatID := rest[:second]
		if chatID == "" {
			continue
		}
		t.byChat[chatID] = Process{
			PID:       pid,
			State:     proc.state,
			WtManager: proc.wtManager,
		}
	}
}

func looksLikeCursorAgent(cmd []byte) bool {
	// Exact patterns we've observed in the wild:
	//   - "/.../cursor-agent ..."
	//   - "/.../cursor-agent/versions/<v>/agent --use-system-ca ..."
	switch {
	case bytes.Contains(cmd, []byte("cursor-agent")):
		return true
	case bytes.Contains(cmd, []byte("/agent --use-system-ca")):
		return true
	}
	return false
}

// argv0 returns the first whitespace-separated token of a command line.
func argv0(cmd []byte) []byte {
	if i := bytes.IndexByte(cmd, ' '); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// baseName returns the last path element of p.
func baseName(p []byte) []byte {
	if i := bytes.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// IsLive reports whether a user-facing cursor-agent currently holds the
// chat's store.db open. Used by the resume gate to prevent forking
// duplicate chats.
func (t *Table) IsLive(chatID string) bool {
	_, ok := t.byChat[chatID]
	return ok
}

// IsPaused reports whether the chat's process is in stopped (T) state.
func (t *Table) IsPaused(chatID string) bool {
	p, ok := t.byChat[chatID]
	if !ok {
		return false
	}
	return p.State == 'T'
}

// IsWtManager reports whether the chat is bound to the dashboard's own
// wt-manager pane.
func (t *Table) IsWtManager(chatID string) bool {
	p, ok := t.byChat[chatID]
	if !ok {
		return false
	}
	return p.WtManager
}

// PID returns the user-facing cursor-agent PID for a chat, or 0 if dead.
func (t *Table) PID(chatID string) int {
	return t.byChat[chatID].PID
}

// Pause sends SIGSTOP to the chat's process. Returns an error if the
// chat has no live process.
func (t *Table) Pause(chatID string) error {
	pid := t.PID(chatID)
	if pid == 0 {
		return fmt.Errorf("no live cursor-agent process for chat %s", chatID)
	}
	return syscall.Kill(pid, syscall.SIGSTOP)
}

// Resume sends SIGCONT.
func (t *Table) Resume(chatID string) error {
	pid := t.PID(chatID)
	if pid == 0 {
		return fmt.Errorf("no live cursor-agent process for chat %s", chatID)
	}
	return syscall.Kill(pid, syscall.SIGCONT)
}

// TogglePause flips between SIGSTOP and SIGCONT.
func (t *Table) TogglePause(chatID string) error {
	if t.IsPaused(chatID) {
		return t.Resume(chatID)
	}
	return t.Pause(chatID)
}

// Kill sends SIGCONT (in case paused) → SIGTERM. The caller is
// responsible for the SIGKILL fallback if the process doesn't exit
// within a grace period.
func (t *Table) Kill(chatID string) error {
	pid := t.PID(chatID)
	if pid == 0 {
		return fmt.Errorf("no live cursor-agent process for chat %s", chatID)
	}
	_ = syscall.Kill(pid, syscall.SIGCONT)
	return syscall.Kill(pid, syscall.SIGTERM)
}

// HardKill sends SIGKILL.
func (t *Table) HardKill(pid int) error {
	if pid == 0 {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
