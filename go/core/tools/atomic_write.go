package tools

import (
	"fmt"
	"os"
	"path/filepath"
)

// atomicWriteFile commits complete tool output with a same-directory rename.
// A streamed tool draft is UI-only and never reaches this function; readers
// therefore see either the previous complete file or the new complete file,
// never a partially decoded model response.
func atomicWriteFile(path string, content []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".meadow-draft-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err = temporary.Chmod(perm); err != nil {
		return err
	}
	if _, err = temporary.Write(content); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit complete file: %w", err)
	}

	// Persist the directory entry where supported. Failure here does not make
	// the already-atomic rename unsafe for concurrent readers, so it remains a
	// best-effort durability barrier (some filesystems reject directory Sync).
	if directory, openErr := os.Open(dir); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
