package interaction

import (
	"fmt"
	"sort"

	"egg-emulator/internal/config"
)

// Matrix generates every combination of variable values (Cartesian product).
// Variable names are sorted for deterministic ordering across runs.
// Returns [][]string where each element is a []string of "KEY=VALUE" pairs.
//
// Examples:
//   vars = {A: [1,2], B: [x,y]}  →  [[A=1,B=x], [A=1,B=y], [A=2,B=x], [A=2,B=y]]
//   vars = {}                     →  [[]] (one empty test case)
func Matrix(vars map[string]config.VariableRange) [][]string {
	if len(vars) == 0 {
		return [][]string{nil}
	}

	// Sort keys for deterministic output
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var results [][]string
	var current []string

	var backtrack func(index int)
	backtrack = func(index int) {
		if index == len(keys) {
			snapshot := make([]string, len(current))
			copy(snapshot, current)
			results = append(results, snapshot)
			return
		}
		key := keys[index]
		v := vars[key]
		switch {
		case v.Static != "":
			current = append(current, fmt.Sprintf("%s=%s", key, v.Static))
			backtrack(index + 1)
			current = current[:len(current)-1]

		case len(v.Range) > 0:
			for _, val := range v.Range {
				current = append(current, fmt.Sprintf("%s=%s", key, val))
				backtrack(index + 1)
				current = current[:len(current)-1]
			}

		default:
			// Variable defined but no value — skip it (don't inject)
			backtrack(index + 1)
		}
	}

	backtrack(0)
	return results
}

// ExtractVarNames scans a string for {{VAR}} placeholders and adds them to dest.
func ExtractVarNames(content string, dest map[string]bool) {
	// Simple manual scan avoids importing regexp at this level
	for i := 0; i < len(content)-3; i++ {
		if content[i] == '{' && content[i+1] == '{' {
			end := i + 2
			for end < len(content)-1 && isVarChar(content[end]) {
				end++
			}
			if end < len(content)-1 && content[end] == '}' && content[end+1] == '}' {
				name := content[i+2 : end]
				if name != "" {
					dest[name] = true
				}
				i = end + 1
			}
		}
	}
}

func isVarChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') || b == '_'
}
