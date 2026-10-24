package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	coresecrets "github.com/hey-meadow/meadow-code/go/core/secrets"
)

func (d *Document) resolveExternalSecrets() {
	if d == nil {
		return
	}
	secretConfig := mapValue(d.effectiveRaw()["secrets"])
	if len(secretConfig) == 0 {
		return
	}

	base := d.startupEnvironment()
	protected := make(map[string]struct{}, len(d.managedEnv))
	for name := range d.managedEnv {
		protected[name] = struct{}{}
	}

	home := ""
	if strings.TrimSpace(d.Path) != "" {
		home = filepath.Dir(d.Path)
	}
	report := coresecrets.DefaultRegistry().Resolve(
		context.Background(),
		secretConfig,
		coresecrets.Options{
			Home:      home,
			Base:      coresecrets.Environment(base),
			Protected: protected,
		},
	)
	d.secretEnv = report.AppliedValues
	d.secretSource = make(map[string]string, len(report.Provenance))
	for name, provenance := range report.Provenance {
		d.secretSource[name] = provenance.Source
	}

	for _, warning := range report.Warnings {
		d.warnings = append(d.warnings, "secret sources: "+warning)
	}
	for _, conflict := range report.Conflicts {
		d.warnings = append(d.warnings, "secret source conflict: "+conflict)
	}
	for _, source := range report.Sources {
		if source.Result.Error != "" {
			kind := source.Result.ErrorKind
			if kind == "" {
				kind = coresecrets.ErrorInternal
			}
			d.warnings = append(
				d.warnings,
				fmt.Sprintf(
					"secret source %s [%s]: %s",
					source.Label,
					kind,
					source.Result.Error,
				),
			)
		}
		for _, warning := range source.Result.Warnings {
			d.warnings = append(
				d.warnings,
				fmt.Sprintf("secret source %s: %s", source.Label, warning),
			)
		}
	}
	sort.Strings(d.warnings)
}

func (d *Document) startupEnvironment() map[string]string {
	base := map[string]string{}
	for name, value := range d.bootstrapEnv {
		base[name] = value
	}
	for name, value := range d.userEnv {
		base[name] = value
	}
	for _, assignment := range os.Environ() {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			base[name] = value
		}
	}
	for name, value := range d.managedEnv {
		base[name] = value
	}
	return base
}

// StartupEnvironment returns the pre-vault environment snapshot used by the
// resolver. Callers must treat the returned values as secret material.
func (d *Document) StartupEnvironment() map[string]string {
	if d == nil {
		return map[string]string{}
	}
	return d.startupEnvironment()
}
