package secrets

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// AppliedVar records the effective source of one resolved variable.
type AppliedVar struct {
	Name        string
	Source      string
	Shape       Shape
	OverrodeEnv bool
}

// SourceReport records one adapter's fetch and merge outcome without exposing
// any resolved values.
type SourceReport struct {
	Name             string
	Label            string
	Result           FetchResult
	Applied          []string
	SkippedExisting  []string
	SkippedClaimed   []string
	SkippedProtected []string
	SkippedInvalid   []string
}

// ResolveReport is one complete startup resolution pass. AppliedValues is the
// only secret-bearing field and is retained by core/config, not formatted for
// logs or user-facing diagnostics.
type ResolveReport struct {
	Sources       []SourceReport
	AppliedValues map[string]string
	Provenance    map[string]AppliedVar
	Conflicts     []string
	Warnings      []string
}

// Options supplies the lower-priority environment and administrator-protected
// names. Base is copied before use.
type Options struct {
	Home      string
	Base      Environment
	Protected map[string]struct{}
}

// Registry owns a deterministic source order and validates names/schemes.
type Registry struct {
	mu      sync.RWMutex
	order   []string
	sources map[string]Source
}

func NewRegistry() *Registry {
	return &Registry{sources: map[string]Source{}}
}

// DefaultRegistry returns the bundled source set in stable registration order.
func DefaultRegistry() *Registry {
	registry := NewRegistry()
	for _, source := range []Source{
		NewKeychainSource(),
		NewOnePasswordSource(),
		NewBitwardenSource(),
	} {
		if err := registry.Register(source, false); err != nil {
			panic(err)
		}
	}
	return registry
}

// Register adds a source after enforcing API, name, shape, and scheme
// uniqueness. replace may replace the implementation for an existing name but
// never steal another source's URI scheme.
func (r *Registry) Register(source Source, replace bool) error {
	if source == nil {
		return fmt.Errorf("secret source is nil")
	}
	descriptor := source.Descriptor()
	if descriptor.Name == "" ||
		descriptor.Name != strings.ToLower(descriptor.Name) ||
		!validSourceName(descriptor.Name) {
		return fmt.Errorf("invalid secret source name %q", descriptor.Name)
	}
	if descriptor.APIVersion != APIVersion {
		return fmt.Errorf(
			"secret source %q uses API v%d; native runtime requires v%d",
			descriptor.Name,
			descriptor.APIVersion,
			APIVersion,
		)
	}
	if descriptor.Shape != ShapeMapped && descriptor.Shape != ShapeBulk {
		return fmt.Errorf(
			"secret source %q has invalid shape %q",
			descriptor.Name,
			descriptor.Shape,
		)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sources[descriptor.Name]; exists && !replace {
		return fmt.Errorf("secret source %q is already registered", descriptor.Name)
	}
	if descriptor.Scheme != "" {
		for name, existing := range r.sources {
			if name != descriptor.Name &&
				existing.Descriptor().Scheme == descriptor.Scheme {
				return fmt.Errorf(
					"secret scheme %q is already owned by %q",
					descriptor.Scheme,
					name,
				)
			}
		}
	}
	if _, exists := r.sources[descriptor.Name]; !exists {
		r.order = append(r.order, descriptor.Name)
	}
	r.sources[descriptor.Name] = source
	return nil
}

func validSourceName(name string) bool {
	for _, character := range name {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '_' {
			continue
		}
		return false
	}
	return name != ""
}

// Resolve fetches and merges all enabled sources. Source failures are
// contained and recorded; another enabled source still runs.
func (r *Registry) Resolve(
	ctx context.Context,
	config map[string]any,
	options Options,
) ResolveReport {
	report := ResolveReport{
		AppliedValues: map[string]string{},
		Provenance:    map[string]AppliedVar{},
	}
	config = normalizeConfig(config)
	sources, warnings := r.orderedEnabled(config)
	report.Warnings = append(report.Warnings, warnings...)
	if len(sources) == 0 {
		return report
	}

	base := cloneEnvironment(options.Base)
	protected := make(map[string]struct{}, len(options.Protected))
	for name := range options.Protected {
		protected[name] = struct{}{}
	}

	type fetchedSource struct {
		source Source
		config map[string]any
		result FetchResult
	}
	fetches := make([]fetchedSource, 0, len(sources))
	for _, source := range sources {
		descriptor := source.Descriptor()
		sourceConfig := sourceConfig(config, descriptor.Name)
		for _, name := range safeProtected(source, sourceConfig) {
			if validEnvName(name) {
				protected[name] = struct{}{}
			}
		}
		fetches = append(fetches, fetchedSource{
			source: source,
			config: sourceConfig,
			result: fetchWithTimeout(
				ctx,
				source,
				FetchRequest{
					Config: sourceConfig,
					Home:   options.Home,
					Env:    cloneEnvironment(base),
				},
			),
		})
	}

	claimed := map[string]string{}
	for _, fetched := range fetches {
		descriptor := fetched.source.Descriptor()
		sourceReport := SourceReport{
			Name:   descriptor.Name,
			Label:  descriptor.Label,
			Result: fetched.result,
		}
		if sourceReport.Label == "" {
			sourceReport.Label = descriptor.Name
		}
		if fetched.result.OK() {
			override := safeOverride(fetched.source, fetched.config)
			names := sortedSecretNames(fetched.result.Secrets)
			for _, name := range names {
				value := fetched.result.Secrets[name]
				if !validEnvName(name) {
					sourceReport.SkippedInvalid = append(
						sourceReport.SkippedInvalid,
						name,
					)
					continue
				}
				if _, isProtected := protected[name]; isProtected {
					sourceReport.SkippedProtected = append(
						sourceReport.SkippedProtected,
						name,
					)
					continue
				}
				if winner, alreadyClaimed := claimed[name]; alreadyClaimed {
					sourceReport.SkippedClaimed = append(
						sourceReport.SkippedClaimed,
						name,
					)
					report.Conflicts = append(
						report.Conflicts,
						fmt.Sprintf(
							"%s: kept value from %s; %s also supplies it "+
								"(first source wins)",
							name,
							winner,
							descriptor.Name,
						),
					)
					continue
				}
				existing := strings.TrimSpace(base[name]) != ""
				if existing && !override {
					sourceReport.SkippedExisting = append(
						sourceReport.SkippedExisting,
						name,
					)
					continue
				}
				claimed[name] = descriptor.Name
				report.AppliedValues[name] = value
				report.Provenance[name] = AppliedVar{
					Name:        name,
					Source:      descriptor.Name,
					Shape:       descriptor.Shape,
					OverrodeEnv: existing,
				}
				sourceReport.Applied = append(sourceReport.Applied, name)
			}
		}
		report.Sources = append(report.Sources, sourceReport)
	}
	return report
}

func normalizeConfig(config map[string]any) map[string]any {
	if config == nil {
		return map[string]any{}
	}
	return config
}

func sourceConfig(config map[string]any, name string) map[string]any {
	value, _ := config[name].(map[string]any)
	if value == nil {
		return map[string]any{}
	}
	return value
}

func (r *Registry) orderedEnabled(
	config map[string]any,
) ([]Source, []string) {
	r.mu.RLock()
	order := append([]string(nil), r.order...)
	sources := make(map[string]Source, len(r.sources))
	for name, source := range r.sources {
		sources[name] = source
	}
	r.mu.RUnlock()

	var warnings []string
	var configured []string
	if entries, ok := config["sources"].([]any); ok {
		for _, entry := range entries {
			name, ok := entry.(string)
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				continue
			}
			if _, exists := sources[name]; !exists {
				warnings = append(
					warnings,
					fmt.Sprintf("secrets.sources names unknown source %q", name),
				)
				continue
			}
			if !contains(configured, name) {
				configured = append(configured, name)
			}
		}
	}
	for _, name := range order {
		if !contains(configured, name) {
			configured = append(configured, name)
		}
	}

	var mapped []Source
	var bulk []Source
	for _, name := range configured {
		source := sources[name]
		sourceCfg := sourceConfig(config, name)
		enabled := false
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					warnings = append(
						warnings,
						fmt.Sprintf(
							"secret source %q enabled check panicked",
							name,
						),
					)
				}
			}()
			enabled = source.Enabled(sourceCfg)
		}()
		if !enabled {
			continue
		}
		if source.Descriptor().Shape == ShapeMapped {
			mapped = append(mapped, source)
		} else {
			bulk = append(bulk, source)
		}
	}
	return append(mapped, bulk...), warnings
}

func fetchWithTimeout(
	parent context.Context,
	source Source,
	request FetchRequest,
) FetchResult {
	timeout := floatConfig(
		request.Config,
		"timeout_seconds",
		DefaultFetchTimeoutSeconds,
	)
	if timeout <= 0 {
		timeout = DefaultFetchTimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(
		parent,
		time.Duration(timeout*float64(time.Second)),
	)
	defer cancel()

	resultChannel := make(chan FetchResult, 1)
	go func() {
		result := FetchResult{}
		defer func() {
			if recovered := recover(); recovered != nil {
				result = FetchResult{
					Error: fmt.Sprintf(
						"fetch panicked: %v",
						recovered,
					),
					ErrorKind: ErrorInternal,
				}
			}
			resultChannel <- result
		}()
		result = source.Fetch(ctx, request)
	}()

	select {
	case result := <-resultChannel:
		if result.Secrets == nil {
			result.Secrets = map[string]string{}
		}
		return result
	case <-ctx.Done():
		return FetchResult{
			Secrets: map[string]string{},
			Error: fmt.Sprintf(
				"fetch exceeded %.0fs budget; startup continued without this source",
				timeout,
			),
			ErrorKind: ErrorTimeout,
		}
	}
}

func safeProtected(source Source, config map[string]any) (result []string) {
	defer func() {
		if recover() != nil {
			result = nil
		}
	}()
	return source.ProtectedEnvVars(config)
}

func safeOverride(source Source, config map[string]any) (result bool) {
	defer func() {
		if recover() != nil {
			result = false
		}
	}()
	return source.OverrideExisting(config)
}

func cloneEnvironment(source Environment) Environment {
	result := make(Environment, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}

func sortedSecretNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
