package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// cloneReadOnlyWorkspace creates an execution-only workspace generation. The
// caller discards it after one requirement check. On APFS regular files use
// copy-on-write clones; other filesystems fall back to a byte copy.
func cloneReadOnlyWorkspace(root string) (string, func(), error) {
	container, err := os.MkdirTemp("", "meadow-requirement-audit-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(container) }
	destination := filepath.Join(container, "workspace")
	if err := cloneWorkspaceTree(root, destination); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return destination, cleanup, nil
}

func cloneWorkspaceTree(root, destination string) error {
	root = filepath.Clean(root)
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := destination
		if relative != "." {
			if ignoredWorkspaceStatePath(relative) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			target = filepath.Join(destination, relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			if err := cloneWorkspaceFile(path, target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved := link
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), resolved)
			}
			resolved = filepath.Clean(resolved)
			targetRelative, err := filepath.Rel(root, resolved)
			if err != nil || targetRelative == ".." || strings.HasPrefix(targetRelative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("workspace symlink escapes isolated audit root: %s", relative)
			}
			if filepath.IsAbs(link) {
				link = filepath.Join(destination, targetRelative)
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("unsupported workspace entry in isolated audit: %s", relative)
		}
	})
}

func copyWorkspaceFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
