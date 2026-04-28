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
package proctable

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Process represents one live user-facing cursor-agent process.
type Process struct {
	PID       int
	State     byte // ps stat letter: 'R' running, 'S' sleeping, 'T' stopped
	WtManager bool // true if argv contains "agent-monitor wt-manager"
}

// Table maps chat id (UUID string) → Process.
type Table struct {
	byChat map[string]Process
}

// New returns an empty table. Call Refresh before reading.
func New() *Table {
	return &Table{byChat: make(map[string]Process)}
}

// Refresh rebuilds the chat → process mapping. Cheap enough to call once
// per tick (~25ms on a busy machine).
func (t *Table) Refresh() {
	t.byChat = map[string]Process{}

	// Step 1: ps. We want pid, state, command. macOS ps `state` is the
	// canonical single-letter status (R/S/T/Z/...).
	psOut, err := exec.Command("ps", "-Awwo", "pid=,state=,command=").Output()
	if err != nil {
		return
	}

	type cand struct {
		pid       int
		state     byte
		wtManager bool
	}
	var cands []cand

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
		if !looksLikeCursorAgent(cmd) {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		isWtm := bytes.Contains(cmd, []byte("agent-monitor wt-manager"))
		cands = append(cands, cand{pid: pid, state: state, wtManager: isWtm})
	}

	if len(cands) == 0 {
		return
	}

	// Step 2: bulk lsof. `lsof -p p1,p2,…` lists open files for all
	// listed pids in one invocation.
	pidArgs := make([]string, 0, len(cands))
	pidIndex := make(map[int]cand, len(cands))
	for _, c := range cands {
		pidArgs = append(pidArgs, strconv.Itoa(c.pid))
		pidIndex[c.pid] = c
	}
	lsofOut, err := exec.Command("lsof", "-p", strings.Join(pidArgs, ",")).Output()
	if err != nil {
		// lsof exits non-zero if any of the pids exited between the
		// `ps` call and now; the partial stdout is still usable.
		// `Output` only sets err for status, not for partial reads.
		// `exec.Command` swallows partial stdout on err==exit with code,
		// so we have to use CombinedOutput-style behavior:
		ee, _ := err.(*exec.ExitError)
		if ee == nil {
			return
		}
		// fall through with whatever we got (zero bytes if hard fail).
	}

	// Format we care about (lsof default):
	//   COMMAND  PID USER  FD TYPE  DEVICE  SIZE/OFF  NODE  NAME
	// Each line begins with the COMMAND, but we re-parse defensively by
	// looking for "<digit>... ... /path".
	currentPID := 0
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
		currentPID = pid
		name := string(fields[len(fields)-1])
		// We're looking for: .../.cursor/chats/<projectHash>/<chatId>/store.db
		i := strings.Index(name, "/.cursor/chats/")
		if i < 0 {
			continue
		}
		if !strings.HasSuffix(name, "/store.db") {
			continue
		}
		// Walk parts after `chats/`: <projectHash>/<chatId>/store.db
		after := name[i+len("/.cursor/chats/"):]
		// after = "<projectHash>/<chatId>/store.db"
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
		c, ok := pidIndex[currentPID]
		if !ok {
			continue
		}
		t.byChat[chatID] = Process{
			PID:       currentPID,
			State:     c.state,
			WtManager: c.wtManager,
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
