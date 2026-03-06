package interaction

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"egg-emulator/internal/config"
	"egg-emulator/pkg/ansi"
	"github.com/BurntSushi/toml"
)

// compiledInteraction is an Interaction with its pattern pre-compiled.
type compiledInteraction struct {
	config.Interaction
	re *regexp.Regexp
}

// Manager handles pattern matching and response dispatch for a single test run.
// It is safe for concurrent use.
type Manager struct {
	mu           sync.RWMutex
	interactions []compiledInteraction
	errorRes     []*regexp.Regexp
	variables    map[string]string
	ranges       map[string]config.VariableRange
	fired        map[string]bool // once-interactions that have fired this run
	// usedVars tracks every {{VAR}} placeholder that was actually substituted
	// during an interaction response. This is the ground truth for whether an
	// interaction variable was truly needed by the server.
	usedVars map[string]bool
	varRe    *regexp.Regexp
}

// New returns an empty, ready-to-use Manager.
func New() *Manager {
	return &Manager{
		variables: make(map[string]string),
		ranges:    make(map[string]config.VariableRange),
		fired:     make(map[string]bool),
		usedVars:  make(map[string]bool),
		varRe:     regexp.MustCompile(`{{([A-Za-z0-9_]+)}}`),
	}
}

// SetVariables resolves "KEY=VALUE" strings into the variable store.
func (m *Manager) SetVariables(vars []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range vars {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) == 2 {
			m.variables[parts[0]] = parts[1]
		}
	}
}

// AddConfig merges a TestConfig into the manager.
func (m *Manager) AddConfig(cfg *config.TestConfig) {
	if cfg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, inter := range cfg.Interactions {
		if inter.Pattern == "" {
			continue
		}
		re, err := regexp.Compile(inter.Pattern)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] invalid interaction pattern %q: %v\n", inter.Pattern, err)
			continue
		}
		if inter.Kind == "" {
			inter.Kind = config.KindSend
		}
		m.interactions = append(m.interactions, compiledInteraction{inter, re})
	}

	for _, pat := range cfg.Errors {
		if pat == "" {
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] invalid error pattern %q: %v\n", pat, err)
			continue
		}
		m.errorRes = append(m.errorRes, re)
	}

	for k, v := range cfg.Variables {
		m.ranges[k] = v
	}

	sort.SliceStable(m.interactions, func(i, j int) bool {
		return m.interactions[i].Priority > m.interactions[j].Priority
	})
}

// LoadFromDirectory loads and merges all .toml files found under dir.
func (m *Manager) LoadFromDirectory(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".toml" {
			return nil
		}
		var cfg config.TestConfig
		if _, decodeErr := toml.DecodeFile(path, &cfg); decodeErr != nil {
			fmt.Fprintf(os.Stderr, "[WARN] failed to decode %s: %v\n", path, decodeErr)
			return nil
		}
		m.AddConfig(&cfg)
		return nil
	})
}

// CheckResult is returned by Check to communicate special actions to the caller.
type CheckResult struct {
	Fired         bool
	ShouldFail    bool
	ShouldSucceed bool
	FailMessage   string
}

// Check scans cumulative for matching patterns and dispatches responses.
func (m *Manager) Check(cumulative string, w io.Writer) CheckResult {
	clean := ansi.Strip(cumulative)

	m.mu.Lock()
	defer m.mu.Unlock()

	var result CheckResult
	for _, inter := range m.interactions {
		if !inter.re.MatchString(clean) {
			continue
		}
		if inter.Once {
			key := inter.Pattern
			if m.fired[key] {
				continue
			}
			m.fired[key] = true
		}

		result.Fired = true

		switch inter.Kind {
		case config.KindFail:
			result.ShouldFail = true
			result.FailMessage = m.resolveVars(inter.Response)
			return result

		case config.KindSuccess:
			result.ShouldSucceed = true
			return result

		default: // KindSend
			response := m.resolveVars(inter.Response)
			fmt.Printf("\n>>> [INTERACTION] pattern=%q response=%q\n", inter.Pattern, response)
			if w != nil {
				fmt.Fprintln(w, response)
			}
		}
	}
	return result
}

// CheckError returns (line, true) if line matches any registered error pattern.
func (m *Manager) CheckError(line string) (string, bool) {
	clean := ansi.Strip(line)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, re := range m.errorRes {
		if re.MatchString(clean) {
			return clean, true
		}
	}
	return "", false
}

// Ranges returns a copy of the available variable ranges.
func (m *Manager) Ranges() map[string]config.VariableRange {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]config.VariableRange, len(m.ranges))
	for k, v := range m.ranges {
		out[k] = v
	}
	return out
}

// AllInteractions returns a snapshot of all loaded interactions.
func (m *Manager) AllInteractions() []config.Interaction {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]config.Interaction, len(m.interactions))
	for i, ci := range m.interactions {
		out[i] = ci.Interaction
	}
	return out
}

// UsedVars returns a copy of the set of variable names that were actually
// substituted into an interaction response during this run.
// This is the authoritative signal for whether an interaction variable was
// truly needed by the server (i.e. the server prompted for it).
func (m *Manager) UsedVars() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.usedVars))
	for k, v := range m.usedVars {
		out[k] = v
	}
	return out
}

// ResetFired clears once-interaction state and used-var tracking for a fresh run.
func (m *Manager) ResetFired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fired = make(map[string]bool)
	m.usedVars = make(map[string]bool)
}

// resolveVars substitutes {{VAR}} placeholders and records which vars were used.
// Must be called with m.mu held (write lock, since it writes usedVars).
func (m *Manager) resolveVars(s string) string {
	return m.varRe.ReplaceAllStringFunc(s, func(match string) string {
		key := match[2 : len(match)-2]
		if val, ok := m.variables[key]; ok {
			m.usedVars[key] = true // record actual usage
			return val
		}
		return match
	})
}