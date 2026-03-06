package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"egg-emulator/internal/config"
	"egg-emulator/internal/proxy"
	"egg-emulator/internal/runner"
	"egg-emulator/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pterm/pterm"
)

func main() {
	var (
		interactive = flag.Bool("interactive", true, "Enable interactive TUI")
		concurrent  = flag.Int("concurrent", 1, "Number of concurrent tests")
		target      = flag.String("target", "", "Specific egg name or path to a .json file")
		eggsDir     = flag.String("eggs-dir", "./eggs", "Directory containing egg JSON files")
		patternsDir = flag.String("patterns-dir", "./patterns", "Directory containing TOML test patterns")
		logsDir     = flag.String("logs-dir", "./logs", "Directory to store test logs")
		proxyAddr   = flag.String("proxy-addr", ":8080", "Address for the local proxy")
		homeDir     = flag.String("home-dir", "..", "Base directory for local file resolution (where Connect folder resides)")
	)
	flag.Parse()

	// Clean data and logs from previous runs
	os.RemoveAll("./data")  //nolint:errcheck
	os.RemoveAll(*logsDir)   //nolint:errcheck

	if err := os.MkdirAll(*logsDir, 0o755); err != nil {
		pterm.Fatal.Printf("Cannot create logs directory: %v\n", err)
	}

	// ── Start Proxy ──────────────────────────────────────────────────────
	absHome, _ := filepath.Abs(*homeDir)
	p, err := proxy.New(*proxyAddr, absHome, filepath.Join(*logsDir, "proxy.log"))
	if err != nil {
		pterm.Error.Printf("Failed to initialize proxy: %v\n", err)
	} else {
		go func() {
			if err := p.Start(); err != nil {
				pterm.Error.Printf("Proxy error: %v\n", err)
			}
		}()
	}

	allEggs, err := config.LoadEggsFromDir(*eggsDir)
	if err != nil {
		pterm.Fatal.Printf("Failed to load eggs from %s: %v\n", *eggsDir, err)
	}
	if len(allEggs) == 0 {
		pterm.Warning.Printf("No valid eggs found in %s\n", *eggsDir)
	}

	eggsToTest := selectEggs(allEggs, *target, *patternsDir)

	r := runner.New(*eggsDir, *patternsDir, *logsDir, *concurrent)

	if *interactive {
		m := tui.New(r, allEggs)
		program := tea.NewProgram(m, tea.WithAltScreen())
		if _, err := program.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// ── Non-interactive / CI mode ─────────────────────────────────────────
	pterm.DefaultHeader.
		WithFullWidth().
		WithBackgroundStyle(pterm.NewStyle(pterm.BgCyan)).
		WithTextStyle(pterm.NewStyle(pterm.FgBlack)).
		Println("Egg Emulator")

	if len(eggsToTest) == 0 {
		pterm.Warning.Println("No eggs selected for testing.")
		return
	}

	go r.Run(context.Background(), eggsToTest)

	passed, failed := 0, 0
	for p := range r.ProgressChan {
		switch p.Status {
		case runner.StatusRunning:
			pterm.Info.Printf("[%s] %s — %s (%d/%d)\n",
				p.EggName, p.Image, p.Message, p.Current, p.Total)
		case runner.StatusPassed:
			passed++
			pterm.Success.Printf("[%s] %s — Passed\n", p.EggName, p.Image)
		case runner.StatusFailed:
			failed++
			pterm.Error.Printf("[%s] %s — Failed: %s\n", p.EggName, p.Image, p.Message)
		case runner.StatusError:
			failed++
			pterm.Error.Printf("[%s] Error: %s\n", p.EggName, p.Message)
		}
	}

	pterm.DefaultSection.Printf("Results: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// selectEggs filters eggs based on the --target flag.
// If target is empty, only eggs with a matching .toml pattern file are included.
func selectEggs(all []*config.Egg, target, patternsDir string) []*config.Egg {
	if target != "" {
		// Single egg by JSON path
		if strings.HasSuffix(target, ".json") {
			egg, err := config.LoadEgg(target)
			if err != nil {
				pterm.Fatal.Printf("Cannot load egg %s: %v\n", target, err)
			}
			return []*config.Egg{egg}
		}
		// Single egg by name
		for _, e := range all {
			if e.Name == target {
				return []*config.Egg{e}
			}
		}
		pterm.Warning.Printf("No egg named %q found\n", target)
		return nil
	}

	// Default: only eggs that have a .toml pattern file
	patternFiles, _ := filepath.Glob(filepath.Join(patternsDir, "egg", "*.toml"))
	patternNames := make(map[string]bool, len(patternFiles))
	for _, f := range patternFiles {
		name := strings.TrimSuffix(filepath.Base(f), ".toml")
		patternNames[name] = true
	}

	var result []*config.Egg
	for _, e := range all {
		if patternNames[e.Name] {
			result = append(result, e)
		}
	}
	return result
}
