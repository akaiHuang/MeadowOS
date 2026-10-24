//go:build !darwin

package runtimeintegrity

import "fmt"

func verifyPlatform() (Evidence, error) {
	return Evidence{}, failure(CheckPlatform, "", fmt.Errorf("runtime integrity verification is implemented only for macOS"))
}
