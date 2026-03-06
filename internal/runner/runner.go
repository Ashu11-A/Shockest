package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"egg-emulator/internal/config"
	"egg-emulator/internal/docker"
	"egg-emulator/internal/environment"
	"egg-emulator/internal/interaction"
	"egg-emulator/internal/report"
	"egg-emulator/internal/server"
	"egg-emulator/pkg/ansi"
)

// Status represents the result state of an egg test.
type Status string

const (
	StatusPending Status = "Pending"
	StatusRunning Status = "Running"
	StatusPassed  Status = "Passed"
	StatusFailed  Status = "Failed"
	StatusSkipped Status = "Skipped"
	StatusError   Status = "Error"
)

// Progress is a progress event emitted during testing.
type Progress struct {
	EggName     string
	Image       string
	Status      Status
	Current     int
	Total       int
	Message     string
	CPUUsage    float64
	MemoryUsage float64
	MemoryLimit float64
	Activity    string
}

// phase2Result holds intermediate results for Phase-2 expansion.
type phase2Result struct {
	imgName string
	imgURL  string
	envVars []string
}

// Runner orchestrates running tests for a set of eggs.
type Runner struct {
	EggsDir      string
	PatternsDir  string
	LogsDir      string
	Concurrent   int
	ProgressChan chan Progress
	// ReportChan receives a completed *report.EggReport after each egg finishes.
	ReportChan chan *report.EggReport
}

// New creates a Runner with the given concurrency limit.
func New(eggsDir, patternsDir, logsDir string, concurrent int) *Runner {
	if concurrent <= 0 {
		concurrent = 1
	}
	return &Runner{
		EggsDir:     eggsDir,
		PatternsDir: patternsDir,
		LogsDir:     logsDir,
		Concurrent:  concurrent,
		// Channels are initialised here and reused for a single Run invocation.
		// After Run completes they are closed; callers that need to run tests
		// multiple times (e.g. the TUI auto-tester) should create a fresh Runner
		// instance per run so they always observe a matching set of channels.
		ProgressChan: make(chan Progress, 256),
		ReportChan:   make(chan *report.EggReport, 64),
	}
}

// Run processes all provided eggs concurrently up to r.Concurrent at a time.
func (r *Runner) Run(ctx context.Context, eggs []*config.Egg) {
	sem := make(chan struct{}, r.Concurrent)
	var wg sync.WaitGroup

	for _, egg := range eggs {
		wg.Add(1)
		sem <- struct{}{}
		go func(e *config.Egg) {
			defer func() {
				<-sem
				wg.Done()
			}()
			r.processEgg(ctx, e)
		}(egg)
	}

	wg.Wait()
	close(r.ProgressChan)
	close(r.ReportChan)
}

// emit sends a Progress event (non-blocking; drops if channel is full).
func (r *Runner) emit(p Progress) {
	select {
	case r.ProgressChan <- p:
	default:
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Variable classification
// ────────────────────────────────────────────────────────────────────────────

// varClass holds classified variable sets for an egg.
type varClass struct {
	// direct: referenced in startup command, egg config files, or test TOML files.
	// These ALWAYS enter the test matrix because they change the server's behaviour
	// regardless of what the server prints.
	direct map[string]bool

	// interactionOnly: only referenced inside interaction *responses*.
	// These enter the matrix only when the server actually prompts for them
	// (i.e. the interaction fires and the variable is substituted).
	interactionOnly map[string]bool
}

// classifyVars separates direct variables from interaction-only variables.
//
// A variable is "direct" if it appears in:
//   - egg.Startup   (e.g. {{SERVER_JARFILE}})
//   - egg.Config.Files  (e.g. server.properties parser)
//   - any test TOML [[files]] content
//
// A variable is "interaction-only" if its ONLY appearance is inside
// interaction response strings (e.g. response = '{{NODE_VERSION}}').
func (r *Runner) classifyVars(e *config.Egg, testConf *config.TestConfig, im *interaction.Manager) varClass {
	direct := make(map[string]bool)
	inResponse := make(map[string]bool)

	// Scan direct sources
	interaction.ExtractVarNames(e.Startup, direct)
	interaction.ExtractVarNames(e.Config.Files, direct)
	if testConf != nil {
		for _, f := range testConf.Files {
			interaction.ExtractVarNames(f.Content, direct)
		}
	}

	// Scan interaction responses (these may be interaction-only)
	for _, inter := range im.AllInteractions() {
		interaction.ExtractVarNames(inter.Response, inResponse)
	}

	// interaction-only = in responses BUT not in direct sources
	interactionOnly := make(map[string]bool)
	for name := range inResponse {
		if !direct[name] {
			interactionOnly[name] = true
		}
	}

	return varClass{direct: direct, interactionOnly: interactionOnly}
}

// ────────────────────────────────────────────────────────────────────────────
// Per-egg processing
// ────────────────────────────────────────────────────────────────────────────

func (r *Runner) processEgg(ctx context.Context, e *config.Egg) {
	r.emit(Progress{EggName: e.Name, Status: StatusRunning, Message: "Starting..."})

	eggReport := report.NewEggReport(e.Name)

	confPath := filepath.Join(r.PatternsDir, "egg", e.Name+".toml")
	testConf, _ := config.LoadTestConfig(confPath)

	logPath := filepath.Join(r.LogsDir, e.Name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		r.emit(Progress{EggName: e.Name, Status: StatusError, Message: fmt.Sprintf("log file: %v", err)})
		return
	}
	defer logFile.Close()

	baseManager := r.buildBaseManager(testConf)
	vc := r.classifyVars(e, testConf, baseManager)
	allRanges := baseManager.Ranges()

	// ── Build initial (phase-1) test cases ───────────────────────────────────
	type testCase struct {
		imgName         string
		imgURL          string
		envVars         []string // → Docker container (direct vars only)
		interactionVars []string // → interaction manager only (never sent to Docker)
	}

	var cases []testCase
	for imgName, imgURL := range e.DockerImages {
		directVars := r.phase1DirectVars(imgName, vc, allRanges, testConf)
		interactionSeed := r.interactionSeedVars(vc, allRanges)
		for _, m := range interaction.Matrix(directVars) {
			cases = append(cases, testCase{imgName, imgURL, m, interactionSeed})
		}
	}

	// Sort for determinism (map iteration is random)
	sort.Slice(cases, func(i, j int) bool {
		if cases[i].imgName != cases[j].imgName {
			return cases[i].imgName < cases[j].imgName
		}
		return strings.Join(cases[i].envVars, ",") < strings.Join(cases[j].envVars, ",")
	})

	totalTests := len(cases) // may grow dynamically in phase 2
	current := 0
	hasFailures := false

	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		current++

		r.emit(Progress{
			EggName: e.Name,
			Image:   tc.imgName,
			Status:  StatusRunning,
			Current: current,
			Total:   totalTests,
			Message: fmt.Sprintf("Testing %s", tc.imgName),
		})

		dataPath := filepath.Join("data", e.Name)
		os.RemoveAll(dataPath)  //nolint:errcheck
		os.MkdirAll(dataPath, 0o755) //nolint:errcheck

		// envVars go to Docker; interaction manager also gets interactionVars
		// so it can respond to server prompts — but interaction vars are NOT
		// injected into the container environment.
		envVars := mergeEnvVars(e.DefaultEnvVars(), tc.envVars)

		im := r.buildBaseManager(testConf)
		im.SetVariables(envVars)
		im.SetVariables(tc.interactionVars) // interaction-only seed (no Docker)

		files := []config.FileContent{}
		done := ""
		if testConf != nil {
			files = testConf.Files
			done = testConf.Done
		}
		if done == "" {
			done = e.DonePattern()
		}

		fmt.Fprintf(logFile, "\n%s\nIMAGE: %s | ENV: %v\n%s\n\n",
			strings.Repeat("=", 80), tc.imgName, envVars, strings.Repeat("=", 80))

		callback := func(status string, stats environment.Stats, activity string) {
			r.emit(Progress{
				EggName:     e.Name,
				Image:       tc.imgName,
				Status:      StatusRunning,
				Current:     current,
				Total:       totalTests,
				Message:     status,
				CPUUsage:    stats.CPUUsage,
				MemoryUsage: stats.MemoryUsage,
				MemoryLimit: stats.MemoryLimit,
				Activity:    activity,
			})
		}

		start := time.Now()
		testErr := r.runTest(ctx, e, im, envVars, files, done, logFile, 120*time.Second, tc.imgURL, callback)
		elapsed := time.Since(start).Milliseconds()

		// Build report env: direct vars always included; interaction-only vars
		// included ONLY if the server actually prompted for them (usedVars).
		usedVars := im.UsedVars()
		reportEnv := report.ParseEnvVars(envVars)
		for _, kv := range tc.interactionVars {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 && usedVars[parts[0]] {
				reportEnv[parts[0]] = parts[1]
			}
		}

		caseResult := report.CaseResult{
			Image:      tc.imgName,
			EnvVars:    reportEnv,
			DurationMs: elapsed,
		}

		if testErr != nil {
			hasFailures = true
			caseResult.Passed = false
			caseResult.Error = testErr.Error()
		} else {
			caseResult.Passed = true
		}
		eggReport.AddCase(caseResult)

		// ── Phase-2 expansion ─────────────────────────────────────────────
		// usedVars was already computed above when building reportEnv.
		// Expand interaction-only range vars that the server actually asked for.
		if i < totalTests { // guard: only expand from original phase-1 cases
			phase2 := r.phase2Cases(tc.imgName, tc.imgURL, tc.interactionVars, vc, allRanges, usedVars)
			if len(phase2) > 0 {
				for _, p2 := range phase2 {
					// Same direct envVars, different interaction seed
					cases = append(cases, testCase{p2.imgName, p2.imgURL, tc.envVars, p2.envVars})
				}
				totalTests += len(phase2)
				fmt.Fprintf(logFile,
					"[PHASE-2] %d additional case(s) added for image %s (interaction vars used: %v)\n",
					len(phase2), tc.imgName, usedVarNames(usedVars))
			}
		}

		status := StatusPassed
		msg := "Passed"
		if testErr != nil {
			status = StatusFailed
			msg = fmt.Sprintf("Failed: %v", testErr)
		}
		r.emit(Progress{
			EggName: e.Name,
			Image:   tc.imgName,
			Status:  status,
			Current: current,
			Total:   totalTests,
			Message: msg,
		})
	}

	eggReport.Finish()

	reportPath, saveErr := report.SaveEgg(r.LogsDir, eggReport)
	if saveErr != nil {
		fmt.Fprintf(os.Stderr, "[WARN] could not save report for %s: %v\n", e.Name, saveErr)
	} else {
		fmt.Printf("[REPORT] Saved: %s\n", reportPath)
	}

	select {
	case r.ReportChan <- eggReport:
	default:
	}

	if hasFailures {
		r.emit(Progress{EggName: e.Name, Status: StatusFailed, Message: "Completed with failures"})
	} else {
		r.emit(Progress{EggName: e.Name, Status: StatusPassed, Message: "Completed"})
	}
}

// phase1DirectVars builds the variable map for the initial matrix.
// Only DIRECT variables enter the matrix — interaction-only vars are excluded
// entirely so they never appear in the Docker container environment.
func (r *Runner) phase1DirectVars(
	imgName string,
	vc varClass,
	allRanges map[string]config.VariableRange,
	testConf *config.TestConfig,
) map[string]config.VariableRange {
	result := make(map[string]config.VariableRange)
	for name, rng := range allRanges {
		if vc.direct[name] {
			result[name] = rng
		}
		// interaction-only vars are intentionally excluded here
	}
	// Per-image static overrides always win
	if testConf != nil {
		if overrides, ok := testConf.DockerImages[imgName]; ok {
			for varName, val := range overrides {
				result[varName] = config.VariableRange{Static: val}
			}
		}
	}
	return result
}

// interactionSeedVars returns "KEY=firstValue" pairs for all interaction-only
// range variables. These are loaded into the interaction manager so it can
// respond to server prompts, but they are never injected into Docker.
func (r *Runner) interactionSeedVars(
	vc varClass,
	allRanges map[string]config.VariableRange,
) []string {
	var seed []string
	for name := range vc.interactionOnly {
		rng, ok := allRanges[name]
		if !ok {
			continue
		}
		fv := firstValue(rng)
		if fv.Static != "" {
			seed = append(seed, name+"="+fv.Static)
		}
	}
	sort.Strings(seed) // deterministic
	return seed
}

// phase2Cases generates additional interaction-seed slices for interaction-only
// range vars confirmed as used in phase 1. The returned phase2Result.envVars
// contains only the INTERACTION vars with a new value — the caller keeps the
// same direct envVars and only swaps interactionVars.
func (r *Runner) phase2Cases(
	imgName, imgURL string,
	currentInteractionVars []string,
	vc varClass,
	allRanges map[string]config.VariableRange,
	usedVars map[string]bool,
) []phase2Result {
	currentMap := report.ParseEnvVars(currentInteractionVars)

	var extra []phase2Result
	for name := range vc.interactionOnly {
		if !usedVars[name] {
			continue // server never asked → skip
		}
		rng, ok := allRanges[name]
		if !ok || len(rng.Range) <= 1 {
			continue // static or single-value → nothing to expand
		}
		currentVal := currentMap[name]
		for _, val := range rng.Range {
			if val == currentVal {
				continue // already tested
			}
			// Build a new interactionVars slice with this var updated
			newInteraction := make([]string, 0, len(currentInteractionVars))
			replaced := false
			for _, kv := range currentInteractionVars {
				if strings.HasPrefix(kv, name+"=") {
					newInteraction = append(newInteraction, name+"="+val)
					replaced = true
				} else {
					newInteraction = append(newInteraction, kv)
				}
			}
			if !replaced {
				newInteraction = append(newInteraction, name+"="+val)
			}
			extra = append(extra, phase2Result{imgName, imgURL, newInteraction})
		}
	}
	return extra
}

// firstValue returns a VariableRange with only the first value from a Range slice,
// or the Static value if set. Used to seed interaction-only vars in phase 1.
func firstValue(rng config.VariableRange) config.VariableRange {
	if rng.Static != "" {
		return rng
	}
	if len(rng.Range) > 0 {
		return config.VariableRange{Static: rng.Range[0]}
	}
	return rng
}

// usedVarNames returns a sorted slice of keys from a bool map (for logging).
func usedVarNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ────────────────────────────────────────────────────────────────────────────
// Single test execution
// ────────────────────────────────────────────────────────────────────────────

type activityWriter struct {
	file     *os.File
	callback func(string)
}

func (w *activityWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	content := strings.TrimSpace(ansi.Strip(string(p)))
	if content != "" {
		for _, line := range strings.Split(content, "\n") {
			if line = strings.TrimSpace(line); line == "" {
				continue
			}
			if len(line) > 120 {
				line = "..." + line[len(line)-117:]
			}
			w.callback(line)
		}
	}
	return n, err
}

func (r *Runner) runTest(
	ctx context.Context,
	e *config.Egg,
	im *interaction.Manager,
	envVars []string,
	files []config.FileContent,
	done string,
	logFile *os.File,
	timeout time.Duration,
	selectedImage string,
	callback server.StatusCallback,
) error {
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env, err := docker.New(e, envVars, files, selectedImage, r.LogsDir)
	if err != nil {
		return fmt.Errorf("runner: create docker env: %w", err)
	}
	defer env.Terminate(context.Background()) //nolint:errcheck

	installWriter := &activityWriter{
		file: logFile,
		callback: func(activity string) {
			callback("Installing", environment.Stats{}, activity)
		},
	}
	if err := env.Install(testCtx, installWriter); err != nil {
		return fmt.Errorf("runner: install: %w", err)
	}

	srv := server.New(env, im, done)
	return srv.Run(testCtx, logFile, callback)
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────────

func (r *Runner) buildBaseManager(testConf *config.TestConfig) *interaction.Manager {
	im := interaction.New()
	im.LoadFromDirectory(filepath.Join(r.PatternsDir, "global"))  //nolint:errcheck
	im.LoadFromDirectory(filepath.Join(r.PatternsDir, "scripts")) //nolint:errcheck
	im.AddConfig(testConf)
	return im
}

func mergeEnvVars(defaults, overrides []string) []string {
	m := make(map[string]string, len(defaults)+len(overrides))
	for _, v := range defaults {
		if parts := strings.SplitN(v, "=", 2); len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	for _, v := range overrides {
		if parts := strings.SplitN(v, "=", 2); len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	result := make([]string, 0, len(m))
	for k, v := range m {
		result = append(result, k+"="+v)
	}
	return result
}

var _ io.Writer = (*activityWriter)(nil)