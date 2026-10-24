package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

const EnvFileName = ".env"

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func loadEnvFile(path string) (map[string]string, []string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	sanitized, _, _ := sanitizeEnvData(data)
	values, warnings := parseEnv(sanitized)
	for index, warning := range warnings {
		warnings[index] = fmt.Sprintf("%s: %s", path, warning)
	}
	return values, warnings, nil
}

func parseEnv(data []byte) (map[string]string, []string) {
	values := map[string]string{}
	var warnings []string
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, raw, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !envNamePattern.MatchString(key) {
			warnings = append(warnings, fmt.Sprintf("line %d has an invalid environment assignment", lineNumber))
			continue
		}
		value, ok := parseEnvValue(strings.TrimSpace(raw))
		if !ok {
			warnings = append(warnings, fmt.Sprintf("line %d has an unterminated quoted value", lineNumber))
			continue
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		warnings = append(warnings, "scan failed: "+err.Error())
	}
	return values, warnings
}

func parseEnvValue(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if raw[0] == '\'' {
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return "", false
		}
		return raw[1 : len(raw)-1], true
	}
	if raw[0] != '"' {
		return raw, true
	}
	if len(raw) < 2 || raw[len(raw)-1] != '"' {
		return "", false
	}
	quoted := raw[1 : len(raw)-1]
	var value strings.Builder
	for index := 0; index < len(quoted); index++ {
		if quoted[index] == '\\' && index+1 < len(quoted) {
			next := quoted[index+1]
			if next == '\\' || next == '"' {
				value.WriteByte(next)
				index++
				continue
			}
		}
		value.WriteByte(quoted[index])
	}
	return value.String(), true
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
