package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

const (
	// LatestSchemaVersion is the newest public config schema understood by the
	// transitional runtime at the time of this migration slice.
	LatestSchemaVersion = 33
	// NativeSchemaFloor is the earliest schema for which every later data
	// transformation has a native Go implementation.
	NativeSchemaFloor = 0
)

var (
	ErrFutureSchemaVersion = errors.New(
		"config schema is newer than this Meadow build",
	)
)

// SchemaInfo describes whether one document can be migrated by the current
// native binary without skipping a historical transformation.
type SchemaInfo struct {
	Current     int    `json:"current"`
	Latest      int    `json:"latest"`
	NativeFloor int    `json:"native_floor"`
	State       string `json:"state"`
}

// SchemaMigrationReport describes one versioned schema migration.
type SchemaMigrationReport struct {
	Changed            bool     `json:"changed"`
	FromVersion        int      `json:"from_version"`
	ToVersion          int      `json:"to_version"`
	BackupPath         string   `json:"backup_path,omitempty"`
	Paths              []string `json:"paths"`
	EnvKeys            []string `json:"env_keys"`
	EnvFixes           int      `json:"env_fixes"`
	CreatedPaths       []string `json:"created_paths"`
	DisabledMCPServers []string `json:"disabled_mcp_servers"`
	Warnings           []string `json:"warnings"`
}

// SchemaInfo returns the raw on-disk version. A missing config matches the
// transitional runtime's behavior and is considered current without a write.
func (d *Document) SchemaInfo() SchemaInfo {
	info := SchemaInfo{
		Latest:      LatestSchemaVersion,
		NativeFloor: NativeSchemaFloor,
	}
	if d == nil {
		info.State = "unavailable"
		return info
	}
	if !d.Exists {
		info.Current = LatestSchemaVersion
		info.State = "current"
		return info
	}
	info.Current = coerceSchemaVersion(d.Raw["_config_version"])
	switch {
	case info.Current > LatestSchemaVersion:
		info.State = "future"
	case info.Current == LatestSchemaVersion:
		info.State = "current"
	case info.Current >= NativeSchemaFloor:
		info.State = "native"
	}
	return info
}

// MigrateSchema applies every historical data migration through schema 33.
func (d *Document) MigrateSchema() (SchemaMigrationReport, error) {
	info := d.SchemaInfo()
	report := SchemaMigrationReport{
		FromVersion:  info.Current,
		ToVersion:    info.Current,
		Paths:        []string{},
		EnvKeys:      []string{},
		CreatedPaths: []string{},
		Warnings:     []string{},
	}
	if d == nil {
		return report, fmt.Errorf("config document is unavailable")
	}
	if info.State == "future" {
		return report, fmt.Errorf(
			"%w: found %d, latest supported is %d",
			ErrFutureSchemaVersion,
			info.Current,
			LatestSchemaVersion,
		)
	}
	if fixes, err := d.sanitizeEnvFile(); err != nil {
		report.Warnings = append(
			report.Warnings,
			fmt.Sprintf("could not repair the Meadow environment file: %v", err),
		)
	} else if fixes > 0 {
		report.EnvFixes = fixes
		report.Changed = true
	}
	if !d.Exists {
		return report, nil
	}
	migrated := cloneMap(d.Raw)
	paths := make([]string, 0, 8)
	effects := schemaMigrationEffects{}
	applyLegacySchemaMigrations(
		d,
		migrated,
		info.Current,
		&paths,
		&effects,
	)
	if info.Current < 29 {
		for _, subsystem := range []string{"memory", "skills"} {
			values := mapValue(migrated[subsystem])
			if values == nil {
				continue
			}
			old, exists := values["write_mode"]
			if !exists {
				continue
			}
			values = cloneMap(values)
			delete(values, "write_mode")
			approve := false
			if text, ok := old.(string); ok {
				approve = strings.EqualFold(strings.TrimSpace(text), "approve")
			}
			if approve {
				values["write_approval"] = true
			} else {
				delete(values, "write_approval")
			}
			if len(values) == 0 {
				delete(migrated, subsystem)
			} else {
				migrated[subsystem] = values
			}
			paths = append(
				paths,
				subsystem+".write_mode -> "+subsystem+".write_approval",
			)
		}
	}
	if info.Current < 31 {
		agent := mapValue(migrated["agent"])
		if agent == nil {
			agent = map[string]any{}
		} else {
			agent = cloneMap(agent)
		}
		current, exists := agent["verify_on_stop"]
		auto, _ := current.(string)
		if !exists || strings.EqualFold(strings.TrimSpace(auto), "auto") {
			agent["verify_on_stop"] = false
			migrated["agent"] = agent
			paths = append(paths, "agent.verify_on_stop -> false")
		}
	}
	if info.Current < 32 {
		agent := mapValue(migrated["agent"])
		if agent != nil {
			if current, ok := agent["verify_on_stop"].(bool); ok && current {
				agent = cloneMap(agent)
				agent["verify_on_stop"] = false
				migrated["agent"] = agent
				paths = append(paths, "agent.verify_on_stop true -> false")
			}
		}
	}
	if info.Current < 33 {
		delegation := mapValue(migrated["delegation"])
		if delegation != nil {
			if old, exists := delegation["max_async_children"]; exists {
				delegation = cloneMap(delegation)
				delete(delegation, "max_async_children")
				if oldValue, valid := migrationInt(old); valid && oldValue > 3 {
					current, valid := migrationInt(
						delegation["max_concurrent_children"],
					)
					if !valid {
						current = 3
					}
					if oldValue > current {
						delegation["max_concurrent_children"] = oldValue
					}
				}
				migrated["delegation"] = delegation
				paths = append(
					paths,
					"delegation.max_async_children -> delegation.max_concurrent_children",
				)
			}
		}
	}
	migrated, aliasPaths := normalizeLegacyAliases(migrated)
	paths = append(paths, aliasPaths...)
	migrated["_config_version"] = LatestSchemaVersion
	if info.Current != LatestSchemaVersion {
		paths = append(paths, "_config_version -> 33")
	}
	disabled, maintenanceWarnings := applyPostMigrationMaintenance(
		migrated,
		&paths,
	)
	report.DisabledMCPServers = disabled
	report.Warnings = append(report.Warnings, maintenanceWarnings...)

	if !reflect.DeepEqual(migrated, d.Raw) {
		original, err := os.ReadFile(d.Path)
		if err != nil {
			return report, fmt.Errorf(
				"read pre-migration config %s: %w",
				d.Path,
				err,
			)
		}
		backup, err := writeMigrationBackup(d.Path, original)
		if err != nil {
			return report, err
		}
		previous := d.Raw
		d.Raw = migrated
		if err := d.Save(); err != nil {
			d.Raw = previous
			return report, fmt.Errorf(
				"save migrated config (backup: %s): %w",
				backup,
				err,
			)
		}
		report.Changed = true
		report.BackupPath = backup
		report.Paths = paths
	}
	report.ToVersion = LatestSchemaVersion
	for _, name := range effects.clearEnv {
		if err := d.SetEnvValue(name, ""); err != nil {
			report.Warnings = append(
				report.Warnings,
				fmt.Sprintf("could not clear %s: %v", name, err),
			)
			continue
		}
		report.EnvKeys = append(report.EnvKeys, name)
	}
	for _, path := range effects.createDirs {
		if err := os.MkdirAll(path, 0o700); err != nil {
			report.Warnings = append(
				report.Warnings,
				fmt.Sprintf("could not create %s: %v", path, err),
			)
			continue
		}
		if err := os.Chmod(path, 0o700); err != nil {
			report.Warnings = append(
				report.Warnings,
				fmt.Sprintf("could not secure %s: %v", path, err),
			)
			continue
		}
		report.CreatedPaths = append(report.CreatedPaths, path)
	}
	if len(report.EnvKeys) > 0 || len(report.CreatedPaths) > 0 {
		report.Changed = true
	}
	return report, nil
}

// MigrateRecentSchema is kept as a source-compatible alias for the first
// native migration slice.
func (d *Document) MigrateRecentSchema() (SchemaMigrationReport, error) {
	return d.MigrateSchema()
}

func coerceSchemaVersion(value any) int {
	if _, ok := value.(bool); ok {
		return 0
	}
	number, valid := migrationInt(value)
	if !valid || number < 0 {
		return 0
	}
	return number
}

func migrationInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case uint:
		return int(typed), true
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		return int(typed), true
	case uint64:
		if uint64(int(typed)) != typed {
			return 0, false
		}
		return int(typed), true
	case float32:
		return int(typed), true
	case float64:
		return int(typed), true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		return number, err == nil
	default:
		return 0, false
	}
}
