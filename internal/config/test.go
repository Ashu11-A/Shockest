package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// InteractionKind defines what action to perform when a pattern matches.
type InteractionKind string

const (
	// KindSend sends the response string to the container's stdin (default).
	KindSend InteractionKind = "send"
	// KindFail immediately fails the test with the response as the error message.
	KindFail InteractionKind = "fail"
	// KindSuccess immediately passes the test (overrides the done pattern).
	KindSuccess InteractionKind = "success"
)

// Interaction defines a single pattern → action rule.
type Interaction struct {
	// Pattern is a Go regular expression matched against the clean log buffer.
	Pattern string `toml:"pattern"`
	// Response is sent to stdin (after variable substitution) when Kind == KindSend.
	Response string `toml:"response"`
	// Kind controls what happens on a match. Defaults to KindSend.
	Kind InteractionKind `toml:"kind"`
	// Once ensures this interaction fires at most once per test run.
	Once bool `toml:"once"`
	// Priority determines check order; higher values are evaluated first.
	Priority int `toml:"priority"`
}

// VariableRange defines the test values for an environment variable.
type VariableRange struct {
	// Static sets a single fixed value; mutually exclusive with Range.
	Static string `toml:"static"`
	// Range lists multiple values; the runner will test every combination (matrix).
	Range []string `toml:"range"`
}

// FileContent describes a file that should be written to the container before start.
type FileContent struct {
	Path    string `toml:"path"`
	Content string `toml:"content"`
}

// TestConfig is the full TOML pattern file for a single egg test.
type TestConfig struct {
	// Name is an optional human-readable label for this test config.
	Name string `toml:"name"`
	// Done overrides the egg's built-in done pattern.
	Done string `toml:"done"`
	// Files lists files to write into the container data directory before startup.
	Files []FileContent `toml:"files"`
	// Variables defines variable ranges for matrix testing.
	Variables map[string]VariableRange `toml:"variables"`
	// Interactions is the ordered list of pattern → action rules.
	Interactions []Interaction `toml:"interactions"`
	// Errors lists regex patterns that, if matched, immediately fail the test.
	Errors []string `toml:"errors"`
	// DockerImages allows per-image variable overrides:
	//   [docker_images."Image Name"]
	//   SOME_VAR = "override_value"
	DockerImages map[string]map[string]string `toml:"docker_images"`
}

// LoadTestConfig loads a TestConfig from path.
// Returns (nil, error) if the file doesn't exist or can't be parsed.
func LoadTestConfig(path string) (*TestConfig, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("test config not found: %s", path)
	}
	var cfg TestConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("parsing test config %s: %w", path, err)
	}
	return &cfg, nil
}
