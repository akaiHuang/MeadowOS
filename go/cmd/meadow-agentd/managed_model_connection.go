//go:build !meadow_protected

package main

import (
	"context"
	"errors"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

type managedAccessAuthority interface {
	ResolveModelAccessForAccountRef(context.Context, string, string, string) (string, error)
}

func managedConnectionCredentialResolver(authority any, connection modelconnection.Connection) provider.CredentialResolver {
	return func(ctx context.Context, baseURL, model string) (string, error) {
		access, ok := authority.(managedAccessAuthority)
		if !ok {
			return "", errors.New("managed model access service is unavailable")
		}
		if strings.TrimRight(baseURL, "/") != connection.BaseURL {
			return "", errors.New("managed model endpoint changed")
		}
		if _, ok := connection.LookupModel(model); !ok {
			return "", errors.New("managed model is not configured")
		}
		accountRef := strings.TrimSpace(connection.AccountRef)
		if accountRef == "" || modelconnection.ManagedConnectionID(accountRef, connection.BaseURL) != connection.ID {
			return "", errors.New("managed connection account binding is unavailable; reconnect Meadow API")
		}
		return access.ResolveModelAccessForAccountRef(ctx, accountRef, connection.BaseURL, model)
	}
}

var _ managedAccessAuthority = (*productauth.Service)(nil)
