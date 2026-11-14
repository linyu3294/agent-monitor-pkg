package wtbridge

import (
	"os"
	"testing"
)

// TestRealMachineProbe is a manual, read-only probe against the repos on
// this machine. Skipped unless AM_PROBE_REPO is set.
func TestRealMachineProbe(t *testing.T) {
	repo := os.Getenv("AM_PROBE_REPO")
	if repo == "" {
		t.Skip("set AM_PROBE_REPO to probe a real checkout")
	}
	t.Logf("DefaultBase(%s) = %q", repo, DefaultBase(repo))

	b := New()
	t.Logf("wt available = %v", b.Available())

	entries, err := os.ReadDir(repo + "/.worktrees")
	if err != nil {
		t.Fatalf("read .worktrees: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		got, err := b.PathFor(repo, e.Name())
		if err != nil {
			t.Errorf("PathFor(%q): %v", e.Name(), err)
			continue
		}
		t.Logf("PathFor(%q) = %q", e.Name(), got)
	}
}
