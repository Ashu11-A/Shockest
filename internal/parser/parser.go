package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// parserKind mirrors Pterodactyl's configuration parser types.
type parserKind string

const (
	parserFile       parserKind = "file"
	parserProperties parserKind = "properties"
	parserJSON       parserKind = "json"
	parserYAML       parserKind = "yaml"
	parserYML        parserKind = "yml"
	parserINI        parserKind = "ini"
	parserXML        parserKind = "xml"
)

// ApplyConfig processes a single file entry from Egg.Config.Files.
// fileName is relative to dataPath.
// configData must be the decoded JSON object for that file entry.
// envVars is the slice of "KEY=VALUE" environment variable strings.
func ApplyConfig(fileName string, configData interface{}, envVars []string, dataPath string) error {
	filePath := filepath.Join(dataPath, fileName)

	// Ensure the file exists (Pterodactyl creates it if missing)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return fmt.Errorf("parser: mkdir %s: %w", filepath.Dir(filePath), err)
		}
		if err := os.WriteFile(filePath, []byte(""), 0o644); err != nil {
			return fmt.Errorf("parser: create %s: %w", filePath, err)
		}
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("parser: read %s: %w", fileName, err)
	}

	cfgMap, ok := configData.(map[string]interface{})
	if !ok {
		return fmt.Errorf("parser: invalid config structure for %s", fileName)
	}

	kind := parserFile
	if p, ok := cfgMap["parser"].(string); ok {
		kind = parserKind(strings.ToLower(p))
	}

	switch kind {
	case parserProperties:
		return applyProperties(filePath, data, cfgMap, envVars)
	case parserJSON:
		return applyJSON(filePath, data, cfgMap, envVars)
	case parserYAML, parserYML:
		return applyText(filePath, data, cfgMap, envVars)
	case parserINI, parserXML, parserFile:
		return applyText(filePath, data, cfgMap, envVars)
	default:
		return applyText(filePath, data, cfgMap, envVars)
	}
}

// ApplyAllConfigs parses the egg's config.files JSON blob and applies each entry.
func ApplyAllConfigs(configFilesJSON string, envVars []string, dataPath string) error {
	if configFilesJSON == "" {
		return nil
	}
	var configFiles map[string]interface{}
	if err := json.Unmarshal([]byte(configFilesJSON), &configFiles); err != nil {
		return fmt.Errorf("parser: decode egg config files JSON: %w", err)
	}
	for fileName, cfg := range configFiles {
		if err := ApplyConfig(fileName, cfg, envVars, dataPath); err != nil {
			return fmt.Errorf("parser: apply %s: %w", fileName, err)
		}
	}
	return nil
}

// resolveValue replaces Pterodactyl-style {{server.build.env.VAR}} and {{VAR}}
// placeholders with their environment variable values.
func resolveValue(value string, envVars []string) string {
	for _, env := range envVars {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k, v := parts[0], parts[1]
		value = strings.ReplaceAll(value, "{{server.build.env."+k+"}}", v)
		value = strings.ReplaceAll(value, "{{"+k+"}}", v)
	}
	// Default Pterodactyl built-ins
	value = strings.ReplaceAll(value, "{{server.build.default.port}}", "25565")
	return value
}

// applyText handles the "file" parser: line-based find-and-replace.
func applyText(filePath string, data []byte, cfg map[string]interface{}, envVars []string) error {
	find, ok := cfg["find"].(map[string]interface{})
	if !ok {
		return nil // nothing to do
	}
	lines := strings.Split(string(data), "\n")
	for key, replacement := range find {
		replStr := resolveValue(fmt.Sprintf("%v", replacement), envVars)
		found := false
		for i, line := range lines {
			if strings.Contains(line, key) {
				lines[i] = replStr
				found = true
			}
		}
		if !found {
			lines = append(lines, replStr)
		}
	}
	newContent := []byte(strings.Join(lines, "\n"))
	err := os.WriteFile(filePath, newContent, 0o644)
	if err != nil {
		// Robustness: If write fails (likely permission denied), try to remove and recreate
		_ = os.Remove(filePath)
		err = os.WriteFile(filePath, newContent, 0o644)
	}
	return err
}

// applyProperties handles the "properties" parser: key=value substitution.
func applyProperties(filePath string, data []byte, cfg map[string]interface{}, envVars []string) error {
	find, ok := cfg["find"].(map[string]interface{})
	if !ok {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	for key, replacement := range find {
		replValue := resolveValue(fmt.Sprintf("%v", replacement), envVars)
		found := false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, key+"=") || strings.HasPrefix(trimmed, key+" =") {
				lines[i] = key + "=" + replValue
				found = true
				break
			}
		}
		if !found {
			lines = append(lines, key+"="+replValue)
		}
	}
	newContent := []byte(strings.Join(lines, "\n"))
	err := os.WriteFile(filePath, newContent, 0o644)
	if err != nil {
		// Robustness: If write fails (likely permission denied), try to remove and recreate
		_ = os.Remove(filePath)
		err = os.WriteFile(filePath, newContent, 0o644)
	}
	return err
}

// applyJSON handles the "json" parser.
// Currently delegates to applyText for broad compatibility;
// full JSONPath replacement can be added per egg requirement.
func applyJSON(filePath string, data []byte, cfg map[string]interface{}, envVars []string) error {
	// Validate that the file contains parseable JSON before modifying
	if len(data) > 0 {
		var probe interface{}
		if err := json.Unmarshal(data, &probe); err != nil {
			// File contains invalid JSON — fall back to text replacement
			return applyText(filePath, data, cfg, envVars)
		}
	}
	return applyText(filePath, data, cfg, envVars)
}
