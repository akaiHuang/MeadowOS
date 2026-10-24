package provider

import (
	"context"
	"errors"
	"strings"
)

// CredentialResolver is a native-only authority for short-lived credentials.
// The current immutable endpoint/model are supplied for origin and allowlist
// checks. Returned credentials are request-local and never stored on Client.
type CredentialResolver func(context.Context, string, string) (string, error)
type resolvedCredentialKey struct{}

func (c *Client) resolveRequestCredential(ctx context.Context, model string) (string, error) {
	if key, ok := ctx.Value(resolvedCredentialKey{}).(string); ok {
		return key, nil
	}
	if c.CredentialResolver == nil {
		return c.APIKey, nil
	}
	if model == "" {
		model = c.Model
	}
	key, err := c.CredentialResolver(ctx, c.BaseURL, model)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("managed model access is unavailable; sign in or refresh the connection")
	}
	if strings.TrimSpace(key) == "" || len(key) > 8192 || strings.ContainsAny(key, "\r\n\x00") {
		return "", errors.New("managed model access returned an invalid credential")
	}
	return key, nil
}
