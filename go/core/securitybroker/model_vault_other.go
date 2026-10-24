//go:build !darwin || !cgo

package securitybroker

import (
	"context"

	"github.com/hey-meadow/meadow-code/go/core/modelvault"
)

func newPlatformModelVaultBackend() (modelVaultBackend, error) {
	return nil, ErrModelVaultUnavailable
}

func destroyPlatformModelVaultProductKeys(context.Context, modelvault.RemovalRequest) error {
	return ErrModelVaultUnavailable
}
