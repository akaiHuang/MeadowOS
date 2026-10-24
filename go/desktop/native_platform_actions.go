//go:build !meadow_protected

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var (
	errDesktopActionUnsupported = errors.New("desktop action is unsupported on this platform")
	errDesktopActionUnavailable = errors.New("desktop application is unavailable")
	errDesktopActionInvalid     = errors.New("invalid desktop action request")
)

// Only these applications can be selected by a renderer. A supplied editor
// value is never treated as an executable name or a shell command.
var desktopEditors = []struct {
	name     string
	commands []string
}{
	{"Visual Studio Code", []string{"code", "code-oss"}},
	{"Cursor", []string{"cursor"}},
	{"Zed", []string{"zed", "zeditor"}},
}

type desktopPlatformActions struct {
	goos     string
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
	run      func(string, ...string) ([]byte, error)
	start    func(string, ...string) error
}

func newDesktopPlatformActions() desktopPlatformActions {
	return desktopPlatformActions{
		goos: runtime.GOOS, lookPath: exec.LookPath, stat: os.Stat,
		run: func(command string, args ...string) ([]byte, error) {
			return exec.Command(command, args...).CombinedOutput()
		},
		start: func(command string, args ...string) error {
			// Linux desktop launchers can stay alive for the entire application
			// lifetime. A successful response acknowledges dispatch, not window
			// visibility; do not hold an HTTP request until the user closes it.
			cmd := exec.Command(command, args...)
			if err := cmd.Start(); err != nil {
				return err
			}
			go func() { _ = cmd.Wait() }()
			return nil
		},
	}
}

func (a desktopPlatformActions) editors() []string {
	found := []string{}
	for _, editor := range desktopEditors {
		if a.goos == "darwin" {
			if _, err := a.stat("/Applications/" + editor.name + ".app"); err == nil {
				found = append(found, editor.name)
			}
		} else if a.goos == "linux" {
			for _, command := range editor.commands {
				if _, err := a.lookPath(command); err == nil {
					found = append(found, editor.name)
					break
				}
			}
		}
	}
	return found
}

func desktopActionPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || strings.ContainsRune(raw, '\x00') {
		return "", fmt.Errorf("%w: path required", errDesktopActionInvalid)
	}
	// An absolute path cannot become an option, even when the original
	// filename begins with a dash. Spaces and shell metacharacters stay data.
	path, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errDesktopActionInvalid, err)
	}
	return path, nil
}

func (a desktopPlatformActions) openEditor(rawPath, requested string) (string, error) {
	if a.goos != "darwin" && a.goos != "linux" {
		return "", errDesktopActionUnsupported
	}
	path, err := desktopActionPath(rawPath)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(requested)
	if name == "" {
		if found := a.editors(); len(found) > 0 {
			name = found[0]
		} else {
			return "", fmt.Errorf("%w: 找不到可用的編輯器（Visual Studio Code / Cursor / Zed）", errDesktopActionUnavailable)
		}
	}
	for _, editor := range desktopEditors {
		if editor.name != name {
			continue
		}
		if a.goos == "darwin" {
			out, err := a.run("open", "-a", name, path)
			return name, desktopActionCommandError("open editor", out, err)
		}
		for _, command := range editor.commands {
			binary, err := a.lookPath(command)
			if err == nil {
				return name, desktopActionCommandError("open editor", nil, a.start(binary, "--", path))
			}
		}
		return "", fmt.Errorf("%w: %s", errDesktopActionUnavailable, name)
	}
	return "", fmt.Errorf("%w: unknown editor %q", errDesktopActionInvalid, name)
}

// reveal uses the session file manager's standard ShowItems API. If the session
// bus/file-manager integration is absent, opening the containing directory is
// still useful; never pass a document to xdg-open because that executes its
// associated application instead of revealing it.
func (a desktopPlatformActions) reveal(rawPath string) (string, error) {
	if a.goos != "darwin" && a.goos != "linux" {
		return "", errDesktopActionUnsupported
	}
	path, err := desktopActionPath(rawPath)
	if err != nil {
		return "", err
	}
	if a.goos == "darwin" {
		out, err := a.run("open", "-R", path)
		return "item", desktopActionCommandError("reveal path", out, err)
	}
	info, err := a.stat(path)
	if err != nil {
		return "", fmt.Errorf("reveal path: %w", err)
	}
	var selectionErr error
	if binary, err := a.lookPath("gdbus"); err == nil {
		uri := (&url.URL{Scheme: "file", Path: path}).String()
		out, err := a.run(binary, "call", "--session", "--timeout", "5",
			"--dest", "org.freedesktop.FileManager1", "--object-path", "/org/freedesktop/FileManager1",
			"--method", "org.freedesktop.FileManager1.ShowItems", "["+strconv.Quote(uri)+"]", "")
		if err == nil {
			return "item", nil
		}
		selectionErr = desktopActionCommandError("select file", out, err)
	}
	directory := path
	if !info.IsDir() {
		directory = filepath.Dir(path)
	}
	if err := a.startLinuxApplication("xdg-open", directory); err != nil {
		return "", errors.Join(selectionErr, err)
	}
	return "directory", nil
}

func (a desktopPlatformActions) openURL(raw string) error {
	value := strings.TrimSpace(raw)
	u, err := url.ParseRequestURI(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w: only valid http(s) URLs are allowed", errDesktopActionInvalid)
	}
	switch a.goos {
	case "darwin":
		out, err := a.run("open", value)
		return desktopActionCommandError("open URL", out, err)
	case "linux":
		return a.startLinuxApplication("xdg-open", value)
	default:
		return errDesktopActionUnsupported
	}
}

func (a desktopPlatformActions) startLinuxApplication(command string, args ...string) error {
	binary, err := a.lookPath(command)
	if err != nil {
		return fmt.Errorf("%w: %s", errDesktopActionUnavailable, command)
	}
	return desktopActionCommandError(command, nil, a.start(binary, args...))
}

func desktopActionCommandError(action string, out []byte, err error) error {
	if err == nil {
		return nil
	}
	if message := strings.TrimSpace(string(out)); message != "" {
		return fmt.Errorf("%s: %s: %w", action, message, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func desktopActionHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errDesktopActionInvalid):
		return http.StatusBadRequest
	case errors.Is(err, errDesktopActionUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, errDesktopActionUnavailable):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
