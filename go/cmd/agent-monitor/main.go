// Command agent-monitor is the dashboard binary.
//
// Modes:
//
//	(default)        run the bubbletea TUI
//	--list           plain-text dump of active sessions (for `am list`)
//	--self-test      run an internal scan + classify smoke test, print result + exit
//	--profile        run a 5-tick benchmark, print per-tick latency, exit
//	--version        print version + exit
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agent-monitor/agent-monitor/internal/classifier"
	"github.com/agent-monitor/agent-monitor/internal/proctable"
	"github.com/agent-monitor/agent-monitor/internal/scanner"
	"github.com/agent-monitor/agent-monitor/internal/tui"
)

// version is overridden at link time via `-ldflags="-X main.version=..."`.
var version = "dev"

func main() {
	listMode := flag.Bool("list", false, "print active sessions as plain text and exit")
	explain := flag.String("explain", "", "explain how a Codewhale session's workspace was resolved, then exit")
	selfTest := flag.Bool("self-test", false, "run an internal smoke test and exit")
	profile := flag.Bool("profile", false, "run a 5-tick benchmark and exit")
	versionFlag := flag.Bool("version", false, "print version and exit")
	refreshMs := flag.Int("refresh-ms", envInt("AM_REFRESH_MS", 2000), "refresh interval in ms")
	idleMin := flag.Int("idle-min", envInt("AM_IDLE_THRESHOLD_MIN", 20), "hide sessions idle longer than this (minutes)")
	maxCards := flag.Int("max-cards", envInt("AM_MAX_CARDS", 50), "max sessions to render")
	flag.Parse()

	switch {
	case *versionFlag:
		fmt.Println(version)
		return
	case *explain != "":
		runExplain(*explain)
		return
	case *selfTest:
		runSelfTest(*idleMin, *maxCards)
		return
	case *profile:
		runProfile(*idleMin, *maxCards)
		return
	case *listMode:
		runList(*idleMin, *maxCards)
		return
	}

	opts := tui.Options{
		RefreshInterval: time.Duration(*refreshMs) * time.Millisecond,
		IdleThreshold:   time.Duration(*idleMin) * time.Minute,
		MaxCards:        *maxCards,
	}
	prog := tea.NewProgram(tui.New(opts, version), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-monitor:", err)
		os.Exit(1)
	}
}

// resolveSessionLog maps an operator-supplied identifier to a session
// log. Accepts an absolute path, a store-relative filename, a full UUID,
// or the short id that `codewhale sessions` prints — the last is what a
// person actually pastes, so it must work.
func resolveSessionLog(dir, id string) string {
	switch {
	case filepath.IsAbs(id):
		return id
	}
	name := id
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	exact := filepath.Join(dir, name)
	if _, err := os.Stat(exact); err == nil {
		return exact
	}
	// Short-id form: `caa88777`. Refuse to guess when the prefix is
	// ambiguous rather than silently explaining the wrong log.
	matches, err := filepath.Glob(filepath.Join(dir, strings.TrimSuffix(name, ".json")+"*.json"))
	if err != nil || len(matches) != 1 {
		return exact
	}
	return matches[0]
}

func runExplain(id string) {
	dir := os.Getenv("AM_CODEWHALE_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".codewhale", "sessions")
	}
	path := resolveSessionLog(dir, id)
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("session:  %s\n", id)
		fmt.Printf("error:    no session log at %s\n", path)
		fmt.Printf("hint:     `codewhale sessions` lists saved ids; pass any prefix of one\n")
		return
	}
	ev := scanner.ExplainCodewhale(path, strings.TrimSuffix(filepath.Base(path), ".json"))
	fmt.Printf("session:  %s\n", ev.ID)
	fmt.Printf("log:      %s\n", ev.Path)
	fmt.Printf("size:     %d bytes\n", ev.Size)
	if ev.Cwd == "" {
		fmt.Printf("workspace: (unresolved) — neither window yielded an existing directory\n")
		return
	}
	fmt.Printf("window:   %s (head=%d KB, tail=%d KB)\n", ev.Window, 512, 64)
	fmt.Printf("signal:   %s\n", ev.Signal)
	if ev.Key != "" {
		fmt.Printf("key:      %s\n", ev.Key)
	}
	fmt.Printf("workspace: %s\n", ev.Cwd)
	fmt.Printf("project:  %s\n", ev.Project)
	if ev.Signal == "frequent path" {
		fmt.Printf("note:     inferred from path frequency, not stated by the log — verify above\n")
	}
	if len(ev.Candidates) > 0 {
		fmt.Printf("candidates:\n")
		for _, c := range ev.Candidates {
			fmt.Printf("  %5d  %s\n", c.Count, c.Path)
		}
	}
	fmt.Printf("last turn:\n")
	fmt.Printf("  role:   %s\n", ev.LastRole)
	if ev.ScannedTail > 0 {
		fmt.Printf("  scanned: %d bytes backwards\n", ev.ScannedTail)
	}
	if ev.LastTool != "" {
		fmt.Printf("  tool:   %s\n", ev.LastTool)
	}
	if ev.LastText != "" {
		fmt.Printf("  text:   %q\n", truncate(ev.LastText, 100))
	}
	if len(ev.Shapes) > 0 {
		fmt.Printf("record shapes (newest first, keys only):\n")
		for _, s := range ev.Shapes {
			fmt.Printf("  %s\n", s)
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func runList(idleMin, maxCards int) {
	scn := scanner.New(scanner.Options{
		IdleThreshold: time.Duration(idleMin) * time.Minute,
		MaxCards:      maxCards,
	})
	tbl := proctable.New()
	tbl.Refresh()
	sessions := scn.Scan()
	fmt.Printf("%-36s  %-10s  %-7s  %-9s  %-30s  %s\n", "CHAT-ID", "SOURCE", "PID", "STATE", "PROJECT", "WORKTREE")
	fmt.Println("------------------------------------------------------------------------------------------------------------------------")
	for _, s := range sessions {
		if tbl.IsWtManager(s.ID) {
			continue
		}
		s.State = classifier.Classify(s, tbl)
		s.PID = tbl.PID(s.ID)
		wt := s.WorktreeSlug
		if wt == "" {
			wt = "(primary)"
		}
		fmt.Printf("%-36s  %-10s  %-7d  %-9s  %-30s  %s\n", s.ID, s.AgentLabel(), s.PID, s.State, s.Project, wt)
	}
}

func runSelfTest(idleMin, maxCards int) {
	scn := scanner.New(scanner.Options{
		IdleThreshold: time.Duration(idleMin) * time.Minute,
		MaxCards:      maxCards,
	})
	tbl := proctable.New()
	t0 := time.Now()
	tbl.Refresh()
	sessions := scn.Scan()
	dur := time.Since(t0)
	live, paused, wt := 0, 0, 0
	for _, s := range sessions {
		if tbl.IsLive(s.ID) {
			live++
		}
		if tbl.IsPaused(s.ID) {
			paused++
		}
		if tbl.IsWtManager(s.ID) {
			wt++
		}
		s.State = classifier.Classify(s, tbl)
		_ = s.State
	}
	fmt.Printf("self-test ok in %v\n", dur)
	fmt.Printf("  sessions:   %d (recent within %d min)\n", len(sessions), idleMin)
	fmt.Printf("  live:       %d\n", live)
	fmt.Printf("  paused:     %d\n", paused)
	fmt.Printf("  wt-manager: %d\n", wt)
}

func runProfile(idleMin, maxCards int) {
	scn := scanner.New(scanner.Options{
		IdleThreshold: time.Duration(idleMin) * time.Minute,
		MaxCards:      maxCards,
	})
	tbl := proctable.New()
	const ticks = 5
	totals := make([]time.Duration, ticks)
	for i := 0; i < ticks; i++ {
		t0 := time.Now()
		tbl.Refresh()
		_ = scn.Scan()
		totals[i] = time.Since(t0)
	}
	fmt.Println("profile (5 ticks):")
	var sum time.Duration
	for i, d := range totals {
		sum += d
		fmt.Printf("  tick %d: %v\n", i+1, d)
	}
	fmt.Printf("  avg:    %v\n", sum/time.Duration(ticks))
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}
