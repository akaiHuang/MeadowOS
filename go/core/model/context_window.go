package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ResolveContextWindowTokens returns the model's total input/output context
// window. The local model config is authoritative because custom or converted
// weights may intentionally differ from the public model family default.
func ResolveContextWindowTokens(modelPath, modelID string) int {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if strings.Contains(id, "e2b") || strings.Contains(id, "e4b") {
		return 131072
	}

	tier, ok := tierByName(modelID)
	if tokens := contextWindowFromConfig(modelPath); tokens > 0 {
		if ok {
			switch tier.ServedName {
			case tierLightning.ServedName, tier12B.ServedName, tier26B.ServedName,
				tier27B.ServedName, tierMuse30B.ServedName, tier35B.ServedName:
				return min(tokens, 131072)
			}
		}
		return tokens
	}

	if !ok {
		return 0
	}
	switch tier.ServedName {
	case tier9B.ServedName:
		return 1048576
	case tierLightning.ServedName, tier12B.ServedName, tier26B.ServedName,
		tier27B.ServedName, tierMuse30B.ServedName, tier35B.ServedName:
		// Meadow's product workspace is a fixed, visible 128K even when the
		// upstream architecture advertises more. Physical KV grows on demand and
		// compaction owns the ceiling; the TUI must not promise a 262K runtime
		// that Core is deliberately serving at 131,072 tokens.
		return 131072
	default:
		return 0
	}
}

func contextWindowFromConfig(modelPath string) int {
	modelPath = strings.TrimSpace(modelPath)
	if modelPath == "" {
		return 0
	}
	path := modelPath
	if filepath.Base(path) != "config.json" {
		path = filepath.Join(path, "config.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	type configWindow struct {
		MaxPositionEmbeddings int `json:"max_position_embeddings"`
		MaxSequenceLength     int `json:"max_sequence_length"`
		SequenceLength        int `json:"seq_length"`
	}
	var cfg struct {
		configWindow
		TextConfig configWindow `json:"text_config"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return 0
	}
	for _, tokens := range []int{
		cfg.TextConfig.MaxPositionEmbeddings,
		cfg.TextConfig.MaxSequenceLength,
		cfg.TextConfig.SequenceLength,
		cfg.MaxPositionEmbeddings,
		cfg.MaxSequenceLength,
		cfg.SequenceLength,
	} {
		if tokens > 0 {
			return tokens
		}
	}
	return 0
}
