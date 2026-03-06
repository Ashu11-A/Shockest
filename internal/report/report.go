package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ────────────────────────────────────────────────────────────────────────────
// Data structures
// ────────────────────────────────────────────────────────────────────────────

// CaseResult is the outcome of a single image × variable-matrix test case.
type CaseResult struct {
	Image   string            `json:"image"`
	EnvVars map[string]string `json:"env_vars"`
	Passed  bool              `json:"passed"`
	Error   string            `json:"error,omitempty"`
	// Duration in milliseconds
	DurationMs int64 `json:"duration_ms"`
}

// EggReport summarises all test cases run for one egg.
type EggReport struct {
	EggName   string       `json:"egg_name"`
	StartedAt time.Time    `json:"started_at"`
	EndedAt   time.Time    `json:"ended_at"`
	Passed    []CaseResult `json:"passed"`
	Failed    []CaseResult `json:"failed"`
	// Totals for quick parsing
	TotalPassed int `json:"total_passed"`
	TotalFailed int `json:"total_failed"`
}

// Summary is the aggregate report written after an auto-test (or multi-egg) run.
type Summary struct {
	GeneratedAt time.Time    `json:"generated_at"`
	TotalPassed int          `json:"total_passed"`
	TotalFailed int          `json:"total_failed"`
	Eggs        []*EggReport `json:"eggs"`
}

// ────────────────────────────────────────────────────────────────────────────
// Builder helpers
// ────────────────────────────────────────────────────────────────────────────

// NewEggReport creates an EggReport and records StartedAt.
func NewEggReport(eggName string) *EggReport {
	return &EggReport{
		EggName:   eggName,
		StartedAt: time.Now(),
	}
}

// AddCase appends a test case result to the appropriate bucket.
func (r *EggReport) AddCase(result CaseResult) {
	if result.Passed {
		r.Passed = append(r.Passed, result)
		r.TotalPassed++
	} else {
		r.Failed = append(r.Failed, result)
		r.TotalFailed++
	}
}

// Finish stamps EndedAt. Call after all cases for this egg are done.
func (r *EggReport) Finish() {
	r.EndedAt = time.Now()
}

// ────────────────────────────────────────────────────────────────────────────
// Persistence
// ────────────────────────────────────────────────────────────────────────────

// SaveEgg writes the per-egg report to <logsDir>/<eggName>.report.json.
func SaveEgg(logsDir string, r *EggReport) (string, error) {
	return saveJSON(logsDir, r.EggName+".report.json", r)
}

// SaveSummary writes the combined auto-test report to <logsDir>/summary.report.json.
func SaveSummary(logsDir string, eggs []*EggReport) (string, error) {
	passed, failed := 0, 0
	for _, e := range eggs {
		passed += e.TotalPassed
		failed += e.TotalFailed
	}
	s := &Summary{
		GeneratedAt: time.Now(),
		TotalPassed: passed,
		TotalFailed: failed,
		Eggs:        eggs,
	}
	return saveJSON(logsDir, "summary.report.json", s)
}

func saveJSON(dir, filename string, v interface{}) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("report: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, filename)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("report: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("report: write %s: %w", path, err)
	}
	return path, nil
}

// ────────────────────────────────────────────────────────────────────────────
// Utility
// ────────────────────────────────────────────────────────────────────────────

// ParseEnvVars converts []string{"K=V", ...} to map[string]string.
func ParseEnvVars(vars []string) map[string]string {
	m := make(map[string]string, len(vars))
	for _, kv := range vars {
		for i, c := range kv {
			if c == '=' {
				m[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return m
}