package modelvault

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func ensurePrivateDirectory(path string) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("vault directory is not a direct directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return nil
}

func rejectSymlinkComponents(target string) error {
	clean := filepath.Clean(target)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("vault path is not absolute")
	}
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	current := volume + string(os.PathSeparator)
	for _, component := range strings.Split(strings.TrimPrefix(remainder, string(os.PathSeparator)), string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("vault path contains a symlink")
		}
	}
	return nil
}

func directRegularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("vault file is not a direct regular file")
	}
	return os.Open(path)
}

func randomToken() (string, error) {
	var bytes [16]byte
	if _, err := io.ReadFull(rand.Reader, bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func atomicWriteJSON(path string, value any, mode fs.FileMode) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	defer wipeBytes(payload)
	token, err := randomToken()
	if err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), ".state-"+token)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	committed = true
	return nil
}

func secureRemoveTree(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(root)
	}
	if info.Mode().IsRegular() {
		return wipeAndRemoveFile(root, info.Size())
	}
	if !info.IsDir() {
		return os.Remove(root)
	}
	_ = os.Chmod(root, 0o700)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := secureRemoveTree(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(root)
}

func wipeAndRemoveFile(path string, size int64) error {
	if size < 0 {
		return fmt.Errorf("negative file size")
	}
	_ = os.Chmod(path, 0o600)
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	buffer := make([]byte, 1024*1024)
	remaining := size
	for remaining > 0 {
		count := int64(len(buffer))
		if remaining < count {
			count = remaining
		}
		if _, err := file.Write(buffer[:count]); err != nil {
			_ = file.Close()
			wipeBytes(buffer)
			return err
		}
		remaining -= count
	}
	err = file.Sync()
	closeErr := file.Close()
	wipeBytes(buffer)
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Remove(path)
}

func wipeBytes(bytes []byte) {
	clear(bytes)
	runtime.KeepAlive(bytes)
}

func safeInstalledName(version string, manifest [32]byte) string {
	return version + "-" + hex.EncodeToString(manifest[:8])
}

func validateBundleID(bundleID string) bool {
	if len(bundleID) < 3 || len(bundleID) > maxIdentifierBytes ||
		strings.HasPrefix(bundleID, ".") || strings.HasSuffix(bundleID, ".") {
		return false
	}
	parts := strings.Split(bundleID, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !validIdentifier(part) {
			return false
		}
	}
	return true
}
