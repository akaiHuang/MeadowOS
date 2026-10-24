package agentdaemon

import (
	"context"
	"errors"
	"net/http"
)

func backgroundWakeAdmission(dependencies Dependencies) func(context.Context, string) error {
	return func(ctx context.Context, _ string) error {
		// HTTP prompt grants cannot authorize a newly generated internal turn.
		// Until that host supplies an automatic-turn grant adapter, never bypass
		// either paid admission or account proofs. VM-lab BYOK has neither gate.
		if dependencies.RequireVerifiedIdentity || dependencies.RequireAccountAuthentication || dependencies.CommercialMembership != nil {
			return errors.New("automatic_background_prompt_grant_unavailable")
		}
		return ctx.Err()
	}
}

// Preserve the session owner's explicit lifecycle through authentication and
// request-limit wrappers. Closing a browser/event connection does not call it.
type ownedSessionHandler struct {
	http.Handler
	owner interface{ Close() error }
}

func (h *ownedSessionHandler) Close() error { return h.owner.Close() }
