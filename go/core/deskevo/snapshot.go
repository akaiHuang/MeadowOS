package deskevo

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/evolve"
)

type snapshotManifest struct {
	Included int               `json:"included"`
	Files    map[string]string `json:"files"`
	Omitted  map[string]string `json:"omitted"`
	Bytes    int64             `json:"bytes"`
}

var excludedDirs = map[string]bool{".git": true, ".qwen": true, ".codex": true, ".ssh": true, ".local": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".next": true, ".venv": true, "venv": true, "__pycache__": true, ".cache": true, ".DS_Store": true}

func snapshot(source, dest string) (m snapshotManifest, err error) {
	m.Files = map[string]string{}
	m.Omitted = map[string]string{}
	canonical, e := filepath.EvalSymlinks(source)
	if e != nil {
		return m, e
	}
	if canonical != filepath.Clean(source) {
		return m, errors.New("Evo source must be a canonical directory")
	}
	root, e := os.OpenRoot(source)
	if e != nil {
		return m, e
	}
	defer root.Close()
	if e = os.MkdirAll(dest, 0700); e != nil {
		return m, e
	}
	count := 0
	err = filepath.WalkDir(source, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == source {
			return nil
		}
		count++
		if count > 10000 {
			return errors.New("Evo project inventory exceeds 10000 entries; choose a smaller project")
		}
		rel, e := filepath.Rel(source, p)
		if e != nil {
			return e
		}
		name := strings.ToLower(d.Name())
		key := filepath.ToSlash(rel)
		if d.IsDir() {
			if excludedDirs[name] || privateName(name) {
				m.Omitted[key] = "excluded directory"
				return filepath.SkipDir
			}
			return nil
		}
		if privateName(name) {
			m.Omitted[key] = "private configuration"
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
			m.Omitted[key] = "link or special file"
			return nil
		}
		if info.Size() > 1024*1024 || m.Bytes+info.Size() > 32*1024*1024 || m.Included >= 2048 {
			m.Omitted[key] = "snapshot limit"
			return nil
		}
		data, e := root.ReadFile(rel)
		if e != nil {
			return e
		}
		if len(data) > 1024*1024 || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
			m.Omitted[key] = "binary or oversized"
			return nil
		}
		if m.Bytes+int64(len(data)) > 32*1024*1024 {
			m.Omitted[key] = "snapshot limit"
			return nil
		}
		target := filepath.Join(dest, rel)
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(target, data, 0600); e != nil {
			return e
		}
		m.Files[key] = sum(string(data))
		m.Bytes += int64(len(data))
		m.Included++
		return nil
	})
	return
}
func writeFiles(root string, files map[string]string) error {
	total := 0
	if len(files) > 2048 {
		return &evolve.CandidateValidationError{Reason: "candidate file count exceeded"}
	}
	for name, body := range files {
		if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\\") || filepath.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || len(body) > 1024*1024 || !utf8.ValidString(body) {
			return &evolve.CandidateValidationError{Reason: "invalid candidate file"}
		}
		total += len(body)
		if total > 32*1024*1024 {
			return &evolve.CandidateValidationError{Reason: "candidate size exceeded"}
		}
		target := filepath.Join(root, name)
		if e := os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		if e := os.WriteFile(target, []byte(body), 0600); e != nil {
			return e
		}
	}
	return nil
}

// Latest is scoped by the native session's canonical project and durable chat.
func Latest(state, cwd, chat string) (map[string]any, error) {
	p := filepath.Join(state, "evo", "projects", sum(cwd)[:24], "latest-"+sum(chat)[:16]+".json")
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 128*1024 {
		return nil, errors.New("Evo status exceeds bound")
	}
	var value map[string]any
	err = json.Unmarshal(data, &value)
	return value, err
}

func privateName(name string) bool {
	if strings.HasPrefix(name, ".env") || strings.Contains(name, "credential") || strings.Contains(name, "secret") || strings.Contains(name, "token") || strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key") {
		return true
	}
	switch name {
	case ".netrc", ".npmrc", ".pypirc", ".docker", ".kube", ".aws", ".azure", ".gnupg", ".config", ".terraform", "id_rsa", "id_ed25519":
		return true
	}
	return false
}
