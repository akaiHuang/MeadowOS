package productauth

import (
	"context"
	"net/http"

	coreauth "github.com/hey-meadow/meadow-code/go/core/productauth"
)

// Managed access projections travel through the authenticated local Agent.
// API credentials never leave its native provider transport.
func (client *Client) ModelAccessStatus(ctx context.Context) (coreauth.ModelAccessStatus, error) {
	var result coreauth.ModelAccessStatus
	if err := client.call(ctx, http.MethodGet, "/model-access/status", nil, &result); err != nil {
		return coreauth.ModelAccessStatus{}, err
	}
	if result.Validate() != nil {
		return coreauth.ModelAccessStatus{}, invalidResponse()
	}
	return result, nil
}

func (client *Client) ModelAccessUsage(ctx context.Context) (coreauth.ModelAccessUsage, error) {
	var result coreauth.ModelAccessUsage
	if err := client.call(ctx, http.MethodGet, "/model-access/usage", nil, &result); err != nil {
		return coreauth.ModelAccessUsage{}, err
	}
	if result.Validate() != nil {
		return coreauth.ModelAccessUsage{}, invalidResponse()
	}
	return result, nil
}

func (client *Client) ModelAccessProvision(ctx context.Context) (coreauth.ModelAccessProvision, error) {
	var result coreauth.ModelAccessProvision
	if err := client.call(ctx, http.MethodPost, "/model-access/provision", coreauth.RefreshRequest{Schema: coreauth.SchemaV1}, &result); err != nil {
		return coreauth.ModelAccessProvision{}, err
	}
	if result.Validate() != nil {
		return coreauth.ModelAccessProvision{}, invalidResponse()
	}
	return result, nil
}
