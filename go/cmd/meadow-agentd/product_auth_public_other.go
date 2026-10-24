//go:build !meadow_protected && (!darwin || !cgo)

package main

import (
	"context"

	"github.com/hey-meadow/meadow-code/go/core/agentdaemon"
)

func composePublicProductAuth(context.Context) (agentdaemon.ProductAuthService, func() error, error) {
	return nil, func() error { return nil }, nil
}
