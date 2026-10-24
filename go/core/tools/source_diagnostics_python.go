package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func sourceDiagnosticPythonRuntime() (string, error) {
	python := os.Getenv("MEADOW_SOURCE_DIAGNOSTICS_PYTHON")
	if python == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		resources := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "Resources"))
		python = filepath.Join(resources, "runtime", "python", "bin", "python3")
	}
	// An explicit absolute runtime is administrator configuration. Never search
	// PATH or load a Python interpreter or parser from the model's workspace.
	if !filepath.IsAbs(python) {
		return "", fmt.Errorf("trusted Python diagnostic runtime is not configured")
	}
	if info, err := os.Stat(python); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("trusted Python diagnostic runtime is unavailable")
	}
	return python, nil
}

func runPythonSourceDiagnostics(ctx context.Context, source string, report *sourceDiagnosticReport) {
	python, err := sourceDiagnosticPythonRuntime()
	if err != nil {
		report.Reason = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Preserve byte fidelity, including BOM and Python encoding declarations.
	// A JSON string would silently replace invalid UTF-8 before parsing.
	payload, _ := json.Marshal(map[string]string{"source_base64": base64.StdEncoding.EncodeToString([]byte(source))})
	cmd := exec.CommandContext(ctx, python, "-I", "-S", "-B", "-c", sourceDiagnosticPythonRunner)
	cmd.Dir = filepath.Dir(python)
	// Isolated mode excludes project modules and PYTHON* environment settings;
	// do not inherit DYLD injection, site packages, startup hooks or user PATH.
	cmd.Env = []string{"LANG=C.UTF-8"}
	cmd.Stdin = bytes.NewReader(payload)
	var output, stderr sourceDiagnosticOutput
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		report.Reason = "Python diagnostic runtime failed or was cancelled; no validation result"
		return
	}
	var observed sourceDiagnosticReport
	if err := json.Unmarshal(output.Bytes(), &observed); err != nil || observed.Status != "checked" || len(observed.Diagnostics) > 20 || !strings.HasPrefix(observed.Parser, "CPython ") {
		report.Reason = "invalid Python diagnostic runtime response"
		return
	}
	report.Status, report.Diagnostics, report.Truncated, report.Parser = observed.Status, observed.Diagnostics, observed.Truncated, observed.Parser
}
