package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SetEnvValue preserves unrelated .env lines while atomically replacing every
// assignment for one user-owned name with a single owner-only assignment.
func (d *Document) SetEnvValue(name, value string) error {
	if d == nil || strings.TrimSpace(d.Path) == "" {
		return errors.New("config path is unavailable")
	}
	name = strings.TrimSpace(name)
	if !envNamePattern.MatchString(name) {
		return fmt.Errorf("invalid environment name %q", name)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("environment value cannot contain NUL or newlines")
	}
	if _, managed := d.managedEnv[name]; managed {
		return fmt.Errorf(
			"environment key %s is managed by the administrator scope %s",
			name,
			d.managedDir,
		)
	}
	path := filepath.Join(filepath.Dir(d.Path), EnvFileName)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	sanitized, _, _ := sanitizeEnvData(data)
	lines := splitEnvLines(sanitized)
	replacement := name + "=" + quoteEnvValue(value) + "\n"
	output := make([]string, 0, len(lines)+1)
	replaced := false
	for _, line := range lines {
		if envAssignmentName(line) != name {
			output = append(output, line)
			continue
		}
		if !replaced {
			output = append(output, replacement)
			replaced = true
		}
	}
	if !replaced {
		if len(output) > 0 && !strings.HasSuffix(output[len(output)-1], "\n") {
			output[len(output)-1] += "\n"
		}
		output = append(output, replacement)
	}
	if err := writePrivateAtomic(path, []byte(strings.Join(output, ""))); err != nil {
		return err
	}
	if d.userEnv == nil {
		d.userEnv = map[string]string{}
	}
	d.userEnv[name] = value
	return nil
}

// RemoveEnvValue deletes every assignment for one user-owned name while
// preserving comments, ordering, unknown lines, and administrator ownership.
func (d *Document) RemoveEnvValue(name string) (bool, error) {
	if d == nil || strings.TrimSpace(d.Path) == "" {
		return false, errors.New("config path is unavailable")
	}
	name = strings.TrimSpace(name)
	if !envNamePattern.MatchString(name) {
		return false, fmt.Errorf("invalid environment name %q", name)
	}
	if _, managed := d.managedEnv[name]; managed {
		return false, fmt.Errorf(
			"environment key %s is managed by the administrator scope %s",
			name,
			d.managedDir,
		)
	}
	path := filepath.Join(filepath.Dir(d.Path), EnvFileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	sanitized, _, _ := sanitizeEnvData(data)
	lines := splitEnvLines(sanitized)
	output := make([]string, 0, len(lines))
	found := false
	for _, line := range lines {
		if envAssignmentName(line) == name {
			found = true
			continue
		}
		output = append(output, line)
	}
	if !found {
		return false, nil
	}
	if err := writePrivateAtomic(path, []byte(strings.Join(output, ""))); err != nil {
		return false, err
	}
	delete(d.userEnv, name)
	return true, nil
}

func quoteEnvValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func splitEnvLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	raw := strings.SplitAfter(string(bytes.TrimPrefix(
		data,
		[]byte{0xef, 0xbb, 0xbf},
	)), "\n")
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	return raw
}

func envAssignmentName(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") || trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	}
	name, _, found := strings.Cut(trimmed, "=")
	name = strings.TrimSpace(name)
	if !found || !envNamePattern.MatchString(name) {
		return ""
	}
	return name
}

func writePrivateAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(
		directory,
		"."+filepath.Base(path)+".tmp-*",
	)
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	committed = true
	return nil
}
