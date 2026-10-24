package agentdaemon

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

type connectionDiscovery struct {
	control       runtimecontrol.RuntimeControl
	source        ModelSource
	auth          ProductAuthService
	accountStatus func(context.Context) (productauth.Status, error)
	mu            sync.Mutex
	cacheKey      string
	connected     bool
	models        []string
	expires       time.Time
}

// Discovery is native-owned, authenticated and coalesced. Only model IDs leave
// this route; provider keys are resolved on the native client at send time.
func (d *connectionDiscovery) handle(w http.ResponseWriter, r *http.Request) {
	external, ok := d.control.(runtimecontrol.ExternalConnection)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"connectionId": "", "connected": false, "models": []string{}})
		return
	}
	c, err := external.Connection()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"connectionId": "", "connected": false, "models": []string{}})
		return
	}
	model, _, _ := external.ConfiguredModel()
	subject := ""
	if d.auth != nil {
		readStatus := d.auth.Status
		if d.accountStatus != nil {
			readStatus = d.accountStatus
		}
		status, statusErr := readStatus(r.Context())
		if AccountAuthenticationError(status, statusErr) != nil {
			d.mu.Lock()
			d.cacheKey = ""
			d.models = nil
			d.expires = time.Time{}
			d.mu.Unlock()
			writeJSON(w, http.StatusUnauthorized, map[string]any{"connectionId": c.ID, "connected": false, "models": []string{}})
			return
		}
		subject = status.Subject
	}
	key := subject + "\x00" + c.ID + "\x00" + model
	if configured, ok := d.control.(externalConfigurationOwner); ok {
		identity := configured.ConfigurationIdentity()
		key += "\x00" + identity.Fingerprint + "\x00" + strconv.FormatUint(identity.Generation, 10)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cacheKey == key && time.Now().Before(d.expires) {
		writeJSON(w, http.StatusOK, map[string]any{"connectionId": c.ID, "connected": d.connected, "models": d.models})
		return
	}
	d.cacheKey = ""
	d.models = nil
	d.expires = time.Time{}
	unavailable := func() {
		d.cacheKey, d.connected, d.models, d.expires = key, false, []string{}, time.Now().Add(2*time.Second)
		writeJSON(w, http.StatusOK, map[string]any{"connectionId": c.ID, "connected": false, "models": []string{}})
	}
	client, err := d.source.Model(model)
	discovered, ok := client.(interface {
		Models(context.Context) ([]string, error)
	})
	if err != nil || !ok {
		unavailable()
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ids, err := discovered.Models(ctx)
	if err != nil {
		unavailable()
		return
	}
	// A changed gateway catalog is not permission to add unconfigured models.
	allowed := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if _, ok := c.LookupModel(id); ok && !seen[id] {
			allowed = append(allowed, id)
			seen[id] = true
		}
	}
	d.cacheKey, d.connected, d.models, d.expires = key, true, allowed, time.Now().Add(10*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{"connectionId": c.ID, "connected": true, "models": allowed})
}
