//go:build meadow_protected || !darwin || !cgo

package main

import (
	"context"
	"errors"
	"io"
)

func handlePublicProductAuthMigration(_ context.Context, args []string, output io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "account-storage-migrate" {
		return false, nil
	}
	_, _ = io.WriteString(output, "{\"copied\":false,\"lease_current\":false,\"source_version\":0,\"destination_version\":0,\"outcome\":\"not_copied\"}\n")
	return true, errors.New("account storage migration: unsupported edition")
}
