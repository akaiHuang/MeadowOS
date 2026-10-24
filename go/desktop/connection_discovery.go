//go:build !meadow_protected

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
)

const desktopDiscoveryCacheTTL = 5 * time.Second
const desktopDiscoveryFailureTTL = time.Second
const desktopDiscoveryTimeout = 6 * time.Second

type desktopDiscoveryFlight struct {
	generation uint64
	done       chan struct{}
	cancel     context.CancelFunc
	models     []modelconnection.Model
	err        error
}

type desktopDiscoveryCache struct {
	mu         sync.Mutex
	scope      string
	until      time.Time
	models     []modelconnection.Model
	err        error
	flight     *desktopDiscoveryFlight
	generation uint64
}

func (runtime *desktopConnectionRuntime) invalidateDiscovery() {
	runtime.cache.mu.Lock()
	defer runtime.cache.mu.Unlock()
	runtime.cache.reset("")
}

func (cache *desktopDiscoveryCache) reset(scope string) {
	if cache.flight != nil {
		cache.flight.cancel()
	}
	cache.generation++
	cache.scope, cache.until, cache.models, cache.err, cache.flight = scope, time.Time{}, nil, nil, nil
}

// Polling and opening the picker share one short read-only probe. Authentication
// still runs before every HTTP request; a changed account gets a new cache scope.
// Cancelling a poll does not cancel another waiter, but logout cancels the probe.
func (runtime *desktopConnectionRuntime) discoveredModels(ctx context.Context) ([]modelconnection.Model, error) {
	scope := desktopAccountScope(ctx)
	runtime.cache.mu.Lock()
	if runtime.cache.scope != scope {
		runtime.cache.reset(scope)
	}
	if time.Now().Before(runtime.cache.until) {
		models, err := append([]modelconnection.Model(nil), runtime.cache.models...), runtime.cache.err
		runtime.cache.mu.Unlock()
		return models, err
	}
	flight := runtime.cache.flight
	if flight == nil {
		probeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopDiscoveryTimeout)
		flight = &desktopDiscoveryFlight{done: make(chan struct{}), cancel: cancel, generation: runtime.cache.generation}
		runtime.cache.flight = flight
		go func() {
			defer cancel()
			var models []modelconnection.Model
			var err error
			runtime.mu.RLock()
			discover := runtime.discover
			runtime.mu.RUnlock()
			if discover == nil {
				err = errors.New("model discovery is unavailable")
			} else {
				models, err = discover(probeContext)
			}
			if probeContext.Err() != nil {
				models, err = nil, probeContext.Err()
			}
			runtime.cache.mu.Lock()
			defer runtime.cache.mu.Unlock()
			flight.models, flight.err = append([]modelconnection.Model(nil), models...), err
			if runtime.cache.flight == flight {
				runtime.cache.models, runtime.cache.err = append([]modelconnection.Model(nil), models...), err
				ttl := desktopDiscoveryCacheTTL
				if err != nil {
					ttl = desktopDiscoveryFailureTTL
				}
				runtime.cache.until = time.Now().Add(ttl)
				runtime.cache.flight = nil
			}
			close(flight.done)
		}()
	}
	runtime.cache.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		runtime.cache.mu.Lock()
		current := runtime.cache.generation == flight.generation
		runtime.cache.mu.Unlock()
		if !current {
			return nil, errors.New("model discovery was invalidated")
		}
		return append([]modelconnection.Model(nil), flight.models...), flight.err
	}
}

// Agent owns dynamic Meadow credentials and sends only model IDs/connectivity
// to Desktop. This boundary is identical for managed, BYOK and local services.
func desktopAgentModelDiscovery(transport *daemontransport.Client, connection modelconnection.Connection) func(context.Context) ([]modelconnection.Model, error) {
	return func(ctx context.Context) ([]modelconnection.Model, error) {
		if transport == nil {
			return nil, errors.New("model discovery is unavailable")
		}
		request, err := transport.NewRequest(ctx, http.MethodGet, "/agent/connections/models", nil)
		if err != nil {
			return nil, errors.New("model discovery request is unavailable")
		}
		response, err := transport.HTTP.Do(request)
		if err != nil {
			return nil, errors.New("model discovery connection failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("model discovery is unavailable")
		}
		var result struct {
			ConnectionID string   `json:"connectionId"`
			Connected    bool     `json:"connected"`
			Models       []string `json:"models"`
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, (1<<20)+1))
		if err := decoder.Decode(&result); err != nil || result.ConnectionID != connection.ID || !result.Connected || result.Models == nil || len(result.Models) > 512 {
			return nil, errors.New("model discovery response is unavailable")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, errors.New("model discovery response is invalid")
		}
		models := make([]modelconnection.Model, 0, len(result.Models))
		for _, id := range result.Models {
			models = append(models, modelconnection.Model{ID: id})
		}
		// Validate bounded IDs without inferring model capabilities from names.
		copy := connection
		copy.Models = models
		if err := modelconnection.Validate(&modelconnection.Catalog{Version: modelconnection.Version, Connections: []modelconnection.Connection{copy}}); err != nil {
			return nil, errors.New("model discovery response is invalid")
		}
		return models, nil
	}
}
