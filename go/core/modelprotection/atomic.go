package modelprotection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// EncryptFileAtomic encrypts a regular file into a temporary ciphertext file
// in the destination directory, syncs it, and renames it into place only after
// the complete source and container have been validated.
func EncryptFileAtomic(
	ctx context.Context,
	inputPath string,
	outputPath string,
	wrapper KeyWrapper,
	options *EncryptOptions,
) (returnErr error) {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if inputPath == "" || outputPath == "" || wrapper == nil {
		return fmt.Errorf("%w: input path, output path, and wrapper are required", ErrInvalidArgument)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("modelprotection: open input: %w", err)
	}
	inputOpen := true
	defer func() {
		if inputOpen {
			_ = input.Close()
		}
	}()
	inputInfo, err := input.Stat()
	if err != nil {
		return fmt.Errorf("modelprotection: stat input: %w", err)
	}
	if !inputInfo.Mode().IsRegular() || inputInfo.Size() < 0 {
		return fmt.Errorf("%w: input must be a regular file", ErrInvalidArgument)
	}
	if outputInfo, err := os.Stat(outputPath); err == nil {
		if os.SameFile(inputInfo, outputInfo) {
			return fmt.Errorf("%w: input and output identify the same file", ErrInvalidArgument)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("modelprotection: stat output: %w", err)
	}

	directory := filepath.Dir(outputPath)
	base := filepath.Base(outputPath)
	if base == "." || base == string(filepath.Separator) {
		return fmt.Errorf("%w: invalid output path", ErrInvalidArgument)
	}
	temporary, err := os.CreateTemp(directory, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("modelprotection: create temporary output: %w", err)
	}
	temporaryPath := temporary.Name()
	temporaryOpen := true
	committed := false
	defer func() {
		if temporaryOpen {
			_ = temporary.Close()
		}
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := Encrypt(ctx, temporary, input, uint64(inputInfo.Size()), wrapper, options); err != nil {
		return err
	}
	if err := input.Close(); err != nil {
		return fmt.Errorf("modelprotection: close input: %w", err)
	}
	inputOpen = false
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("modelprotection: sync temporary output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("modelprotection: close temporary output: %w", err)
	}
	temporaryOpen = false
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		return fmt.Errorf("modelprotection: commit output: %w", err)
	}
	committed = true
	return nil
}
