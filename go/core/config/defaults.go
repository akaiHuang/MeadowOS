package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// default_config.json is the schema-33 Python DEFAULT_CONFIG snapshot with
// the two product-owned values changed to Meadow identity. Keeping the
// complete tree lets native config get/set validate and coerce every current
// setting without importing Python at runtime.
//
//go:embed default_config.json
var defaultConfigJSON []byte

var (
	defaultConfigOnce sync.Once
	defaultConfigRoot map[string]any
	defaultConfigErr  error
)

func loadDefaultConfig() (map[string]any, error) {
	defaultConfigOnce.Do(func() {
		defaultConfigRoot = map[string]any{}
		if err := json.Unmarshal(defaultConfigJSON, &defaultConfigRoot); err != nil {
			defaultConfigErr = fmt.Errorf("parse embedded Meadow defaults: %w", err)
		}
	})
	if defaultConfigErr != nil {
		return nil, defaultConfigErr
	}
	return defaultConfigRoot, nil
}

// DefaultTree returns an independent copy of the complete native defaults.
func DefaultTree() (map[string]any, error) {
	defaults, err := loadDefaultConfig()
	if err != nil {
		return nil, err
	}
	return cloneMap(defaults), nil
}

// EffectiveTree merges user and administrator values over the complete
// defaults using the same null-section rule as the Python config loader.
func (d *Document) EffectiveTree() (map[string]any, error) {
	defaults, err := loadDefaultConfig()
	if err != nil {
		return nil, err
	}
	return deepMerge(defaults, d.effectiveRaw()), nil
}

// DefaultValue resolves one dotted path from the embedded defaults.
func DefaultValue(dotted string) (any, bool, error) {
	defaults, err := loadDefaultConfig()
	if err != nil {
		return nil, false, err
	}
	value, found := lookupDotted(defaults, dotted)
	return cloneValue(value), found, nil
}

// EffectiveValue resolves a dotted map/list path after defaults and managed
// policy have been applied.
func (d *Document) EffectiveValue(dotted string) (any, bool, error) {
	effective, err := d.EffectiveTree()
	if err != nil {
		return nil, false, err
	}
	value, found := lookupDotted(effective, dotted)
	return cloneValue(value), found, nil
}
