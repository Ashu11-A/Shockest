package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EggVariable describes a configurable environment variable defined in an egg.
type EggVariable struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	EnvVariable  string `json:"env_variable"`
	DefaultValue string `json:"default_value"`
}

// EggInteraction is an interaction defined inside the egg's startup config JSON.
type EggInteraction struct {
	Pattern  string `json:"pattern"`
	Response string `json:"response"`
}

// EggStartupConfig is parsed from Egg.Config.Startup JSON blob.
type EggStartupConfig struct {
	Done            string           `json:"done"`
	UserInteraction []EggInteraction `json:"userInteraction"`
}

// EggScripts holds installation script details.
type EggScripts struct {
	Installation struct {
		Script     string `json:"script"`
		Container  string `json:"container"`
		Entrypoint string `json:"entrypoint"`
	} `json:"installation"`
}

// EggConfig holds the raw JSON blobs embedded in the egg.
type EggConfig struct {
	Files   string `json:"files"`
	Startup string `json:"startup"`
	Logs    string `json:"logs"`
	Stop    string `json:"stop"`
}

// Egg represents a Pterodactyl egg definition (loaded from JSON).
type Egg struct {
	Name         string            `json:"name"`
	Author       string            `json:"author"`
	Description  string            `json:"description"`
	DockerImages map[string]string `json:"docker_images"`
	Startup      string            `json:"startup"`
	Config       EggConfig         `json:"config"`
	Variables    []EggVariable     `json:"variables"`
	Scripts      EggScripts        `json:"scripts"`
}

// StartupConfig parses and returns the embedded startup config JSON.
func (e *Egg) StartupConfig() (EggStartupConfig, error) {
	var sc EggStartupConfig
	if e.Config.Startup == "" {
		return sc, nil
	}
	return sc, json.Unmarshal([]byte(e.Config.Startup), &sc)
}

// DonePattern returns the "done" boot pattern for this egg,
// falling back to "Started" if unset.
func (e *Egg) DonePattern() string {
	sc, err := e.StartupConfig()
	if err != nil || sc.Done == "" {
		return "Started"
	}
	return sc.Done
}

// DefaultEnvVars returns a slice of "KEY=VALUE" strings built from the egg's
// variable defaults, so the environment is always pre-seeded.
func (e *Egg) DefaultEnvVars() []string {
	vars := make([]string, 0, len(e.Variables))
	for _, v := range e.Variables {
		if v.EnvVariable != "" && v.DefaultValue != "" {
			vars = append(vars, fmt.Sprintf("%s=%s", v.EnvVariable, v.DefaultValue))
		}
	}
	return vars
}

// LoadEgg loads and parses a single egg from a JSON file.
func LoadEgg(path string) (*Egg, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading egg %s: %w", path, err)
	}
	var egg Egg
	if err := json.Unmarshal(data, &egg); err != nil {
		return nil, fmt.Errorf("parsing egg %s: %w", path, err)
	}
	return &egg, nil
}

// LoadEggsFromDir walks dir and loads every valid egg JSON found.
// Files that cannot be parsed are silently skipped.
func LoadEggsFromDir(dir string) ([]*Egg, error) {
	var eggs []*Egg
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		egg, err := LoadEgg(path)
		if err != nil {
			return nil // skip unparseable files
		}
		if len(egg.DockerImages) > 0 && egg.Startup != "" {
			eggs = append(eggs, egg)
		}
		return nil
	})
	return eggs, err
}
