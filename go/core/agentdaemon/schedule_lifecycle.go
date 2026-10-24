package agentdaemon

import "context"

// Preserve the product owner's lifecycle through the authenticated HTTP shell.
// Admission still occurs inside the owner's ordinary account/permission gates.
func (h *ownedSessionHandler) StartBackgroundServices(ctx context.Context) error {
	if owner, ok := h.owner.(interface{ StartBackgroundServices(context.Context) error }); ok {
		return owner.StartBackgroundServices(ctx)
	}
	return nil
}

// BYOK configuration adds an outer identity wrapper; embedding http.Handler
// alone does not preserve optional lifecycle methods.
func (h *connectionIdentityHandler) StartBackgroundServices(ctx context.Context) error {
	if owner, ok := h.Handler.(interface{ StartBackgroundServices(context.Context) error }); ok {
		return owner.StartBackgroundServices(ctx)
	}
	return nil
}
