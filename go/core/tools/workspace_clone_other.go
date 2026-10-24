//go:build !darwin

package tools

import "os"

func cloneWorkspaceFile(source, destination string, mode os.FileMode) error {
	return copyWorkspaceFile(source, destination, mode)
}
