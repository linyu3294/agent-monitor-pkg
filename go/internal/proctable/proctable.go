// Package proctable resolves chat ids to user-facing cursor-agent
// processes. The mapping is built once per refresh tick by:
//
//  1. Listing all processes with `ps -Awwo pid,state,command`.
//  2. Keeping pids whose command line looks like a cursor-agent.
//  3. For each candidate pid, looking up open files with `lsof` and
//     extracting the chat id from
//     `~/.cursor/chats/<projectHash>/<chatId>/store.db` (which the
//     user-facing cursor-agent keeps open for-write while the chat is
//     live).
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
package proctable

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Process represents one live user-facing agent process (Cursor or
// Codewhale).
type Process struct {
	PID       int
	State     byte // ps stat letter: 'R' running, 'S' sleeping, 'T' stopped
	WtManager bool // true if argv contains "agent-monitor wt-manager"
}

// Table maps chat id (UUID string) → Process.
type Table struct {
	byChat       map[string]Process
	codewhaleDir string
}

// New returns an empty table. Call Refresh before reading.
func New() *Table {
	return &Table{
		byChat:       make(map[string]Process),
		codewhaleDir: defaultCodewhaleDir(),
	}
}

// agentProc is one live agent process found in this tick's ps snapshot.
type agentProc struct {
	pid       int
	state     byte
	wtManager bool
	codewhale bool
}

// Refresh rebuilds the chat → process mapping. Cheap enough to call once
// per tick (~25ms on a busy machine).
func (t *Table) Refresh() {
	t.byChat = map[string]Process{}

	// Step 1: ps. We want pid, state, command. macOS ps `state` is the
	// canonical single-letter status (R/S/T/Z/...).
	psOut, err := exec.Command("ps", "-Awwo", "pid=,state=,command=").Output()
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
		// "<pid> <state> <cmdline>"
		var pidStr string
		idx := bytes.IndexByte(line, ' ')
		if idx <= 0 {
			continue
		}
		pidStr = string(line[:idx])
		rest := bytes.TrimLeft(line[idx+1:], " ")
		if len(rest) == 0 {
			continue
		}
		state := rest[0]
		// state can be e.g. "S+", we just want the first letter.
		idx2 := bytes.IndexByte(rest, ' ')
		if idx2 < 0 {
			continue
		}
		cmd := bytes.TrimLeft(rest[idx2+1:], " ")
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		if looksLikeCodewhale(cmd) {
			liveCodewhale[pid] = state
			procs = append(procs, agentProc{pid: pid, state: state, codewhale: true})
			continue
		}
		if !looksLikeCursorAgent(cmd) {
			continue
		}
		isWtm := bytes.Contains(cmd, []byte("agent-monitor wt-manager"))
		procs = append(procs, agentProc{pid: pid, state: state, wtManager: isWtm})
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
