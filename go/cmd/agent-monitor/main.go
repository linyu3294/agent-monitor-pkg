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

func runList(idleMin, maxCards int) {
	scn := scanner.New(scanner.Options{
		IdleThreshold: time.Duration(idleMin) * time.Minute,
		MaxCards:      maxCards,
	})
	tbl := proctable.New()
	tbl.Refresh()
	sessions := scn.Scan()
	fmt.Printf("%-36s  %-9s  %-30s  %s\n", "CHAT-ID", "STATE", "PROJECT", "WORKTREE")
	fmt.Println("------------------------------------------------------------------------------------------")
	for _, s := range sessions {
		if tbl.IsWtManager(s.ID) {
			continue
		}
		s.State = classifier.Classify(s, tbl)
		wt := s.WorktreeSlug
		if wt == "" {
			wt = "(primary)"
		}
		fmt.Printf("%-36s  %-9s  %-30s  %s\n", s.ID, s.State, s.Project, wt)
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
