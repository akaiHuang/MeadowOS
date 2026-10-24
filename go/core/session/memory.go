package session

import (
	"os"
	"path/filepath"
	"strings"
)

// memoryFileNames are the user-memory filenames in discovery order. The TS
// install on disk uses QWEN.md at the global level; MEADOW.md and AGENTS.md
// are accepted for the rebrand and the emerging convention respectively.
var memoryFileNames = []string{"memory.md", "MEADOW.md", "QWEN.md", "AGENTS.md"}

type MemoryBundle struct {
	Global  string
	Project string
}

// LoadUserMemory concatenates global memory (home-dir chain) and project
// memory (project root), separated by blank lines. Empty string when none.
func LoadUserMemory(projectRoot string) string {
	bundle := LoadUserMemoryBundle(projectRoot)
	return strings.Join(nonEmptyMemoryParts(bundle.Global, bundle.Project), "\n\n")
}

// LoadUserMemoryBundle preserves memory provenance so ContextManager can apply
// a relevance policy to global memory without dropping repository-local
// instructions.
func LoadUserMemoryBundle(projectRoot string) MemoryBundle {
	var bundle MemoryBundle
	if global := findInChain(memoryFileNames...); global != "" {
		if b, err := os.ReadFile(global); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			bundle.Global = strings.TrimSpace(string(b))
		}
	}
	if projectRoot != "" {
		for _, name := range memoryFileNames {
			p := filepath.Join(projectRoot, name)
			if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
				bundle.Project = strings.TrimSpace(string(b))
				break
			}
		}
	}
	return bundle
}

func nonEmptyMemoryParts(values ...string) []string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return parts
}
