package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type markdownPublicationEntry struct {
	path      string
	staged    string
	backup    string
	old       os.FileInfo
	new       os.FileInfo
	backedUp  bool
	published bool
}

type markdownPublicationError struct {
	Cause        error
	ChangedPaths []string
}

func (e *markdownPublicationError) Error() string { return e.Cause.Error() }
func (e *markdownPublicationError) Unwrap() error { return e.Cause }

// Publishing a compiled bundle owns only these fixed leaves. It stages all
// new bytes, preserves original regular files and rolls back a partial commit
// on any failure/cancellation. It does not replace the surrounding directory
// or depend on a particular agent runtime's transaction implementation.
func publishMarkdownPresentationBundle(ctx context.Context, ws *Workspace, directory string, paths []string, files map[string][]byte, beforePublish func(int, string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(directory, ".markdown-publication-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	entries := make([]markdownPublicationEntry, 0, len(paths))
	seen := map[string]bool{}
	for index, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		resolved, err := resolveCanonicalWithinRoot(path, path, directory, "presentation output")
		if err != nil || resolved != path || seen[resolved] {
			return fmt.Errorf("presentation output changed or aliased during publication")
		}
		seen[resolved] = true
		if err := ws.CheckWritable(path); err != nil {
			return err
		}
		if err := ws.CheckPriorRead(path); err != nil {
			return err
		}
		old, statErr := os.Lstat(path)
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if statErr == nil && !old.Mode().IsRegular() {
			return fmt.Errorf("presentation output is not a regular owned file")
		}
		entry := markdownPublicationEntry{path: path, staged: filepath.Join(staging, fmt.Sprintf("new-%02d", index)), backup: filepath.Join(staging, fmt.Sprintf("old-%02d", index))}
		if statErr == nil {
			entry.old = old
		}
		payload, ok := files[path]
		if !ok {
			return fmt.Errorf("presentation output bytes are incomplete")
		}
		if err := os.WriteFile(entry.staged, payload, 0o644); err != nil {
			return err
		}
		entry.new, err = os.Lstat(entry.staged)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
	}
	rollback := func(cause error) error {
		var failed []string
		for index := len(entries) - 1; index >= 0; index-- {
			entry := &entries[index]
			if entry.published {
				current, err := os.Lstat(entry.path)
				if err != nil && !os.IsNotExist(err) {
					failed = append(failed, entry.path)
					continue // Ownership cannot be checked; preserve the path/backup.
				}
				if err == nil && (entry.new == nil || !os.SameFile(entry.new, current) || !entry.new.ModTime().Equal(current.ModTime()) || entry.new.Size() != current.Size()) {
					failed = append(failed, entry.path)
					continue // Preserve a concurrent outside edit, retaining the backup.
				}
				if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
					failed = append(failed, entry.path)
					continue
				}
			}
			if entry.backedUp {
				if _, err := os.Lstat(entry.path); !os.IsNotExist(err) {
					failed = append(failed, entry.path)
					continue
				}
				if err := os.Rename(entry.backup, entry.path); err != nil {
					failed = append(failed, entry.path)
				}
			}
		}
		if len(failed) != 0 {
			cleanup = false // Retain owned original-file backups for recovery.
			return &markdownPublicationError{Cause: fmt.Errorf("presentation publication failed and rollback needs recovery; original backups retained at %s: %w", ws.ModelPath(staging), cause), ChangedPaths: failed}
		}
		return cause
	}
	for index := range entries {
		entry := &entries[index]
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		if beforePublish != nil {
			if err := beforePublish(index, entry.path); err != nil {
				return rollback(err)
			}
		}
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		resolved, err := resolveCanonicalWithinRoot(entry.path, entry.path, directory, "presentation output")
		if err != nil || resolved != entry.path {
			return rollback(fmt.Errorf("presentation output changed during commit"))
		}
		current, statErr := os.Lstat(entry.path)
		if entry.old == nil {
			if !os.IsNotExist(statErr) {
				return rollback(fmt.Errorf("new presentation output appeared during commit; it was preserved"))
			}
		} else {
			if statErr != nil || !os.SameFile(entry.old, current) || !current.Mode().IsRegular() || !entry.old.ModTime().Equal(current.ModTime()) || entry.old.Size() != current.Size() {
				return rollback(fmt.Errorf("existing presentation output changed during commit; it was preserved"))
			}
			if err := ws.CheckPriorRead(entry.path); err != nil {
				return rollback(err)
			}
			if err := os.Rename(entry.path, entry.backup); err != nil {
				return rollback(err)
			}
			entry.backedUp = true
		}
		if err := os.Rename(entry.staged, entry.path); err != nil {
			return rollback(err)
		}
		entry.published = true
	}
	if err := ctx.Err(); err != nil {
		return rollback(err)
	}
	return nil
}
