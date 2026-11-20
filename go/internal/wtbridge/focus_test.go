package wtbridge

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The device string is interpolated into an AppleScript, so anything that
// is not a plain pty path must be refused before it reaches osascript.
func TestFocusITermTTYRejectsAnythingButATTY(t *testing.T) {
	bad := []string{
		"",
		"ttys001",
		"/dev/ttys001\"; do shell script \"echo pwned",
		"/dev/tty; echo",
		"/dev/../etc/passwd",
		"/dev/ttys001 ",
		"not-a-device",
	}
	for _, device := range bad {
		if err := FocusITermTTY(device); err == nil {
			t.Fatalf("FocusITermTTY(%q) accepted an unsafe device", device)
		}
	}
}

func TestTTYForPIDRejectsNonsense(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if got := TTYForPID(pid); got != "" {
			t.Fatalf("TTYForPID(%d) = %q, want empty", pid, got)
		}
	}
}

// A process running on a pty must report that device; `script` allocates
// one, which is the closest thing to an agent tab this test can make
// without touching the person's iTerm.
func TestTTYForPIDFindsAPty(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script not available")
	}
	cmd := exec.Command("script", "-q", "/dev/null", "sleep", "20")
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start a pty: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// The pty belongs to the child `sleep`, not to `script` itself.
	out, err := exec.Command("ps", "-Awwo", "pid=,tty=,command=").Output()
	if err != nil {
		t.Skipf("ps failed: %v", err)
	}
	child := 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "sleep 20") || strings.Contains(line, "script") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == cmd.Process.Pid {
			continue
		}
		child = pid
	}
	if child == 0 {
		t.Skip("could not identify the pty-backed child process")
	}

	got := TTYForPID(child)
	if !strings.HasPrefix(got, "/dev/tty") {
		t.Fatalf("TTYForPID(%d) = %q, want a /dev/tty device", child, got)
	}
}
