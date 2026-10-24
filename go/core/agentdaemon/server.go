// Package agentdaemon owns the local authenticated Agent server composition.
// It does not listen on a network address; the meadow-agentd command supplies
// the private signed-peer Unix listener.
package agentdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/agenttrace"
	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"github.com/hey-meadow/meadow-code/go/core/edition"
	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"github.com/hey-meadow/meadow-code/go/core/producthost"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

const (
	MaxRequestBytes   int64 = 256 << 20
	MaxEventBytes     int64 = 64 << 20
	MaxActiveRequests       = 128
	MaxHeaderBytes          = 64 << 10
)

var (
	ErrNativeProviderUnavailable = errors.New("protected native inference provider is not wired")
	errEventLimit                = errors.New("Meadow Agent event stream exceeded its bounded response limit")
)

type Config = producthost.Config

type ModelSource interface {
	Model(string) (agentcore.KernelModel, error)
}

type ModelSourceFunc func(string) (agentcore.KernelModel, error)

func (function ModelSourceFunc) Model(modelID string) (agentcore.KernelModel, error) {
	return function(modelID)
}

func UnavailableModelSource(reason error) ModelSource {
	if reason == nil {
		reason = ErrNativeProviderUnavailable
	}
	return ModelSourceFunc(func(string) (agentcore.KernelModel, error) { return nil, reason })
}

type Dependencies struct {
	TraceObserver                agenttrace.Observer
	RuntimeControl               runtimecontrol.RuntimeControl
	ModelSource                  ModelSource
	Browser                      BrowserOwner
	ProductUpdate                ProductUpdateService
	OfficialModels               OfficialModelService
	ProductAuth                  ProductAuthService
	ProductAuthOptional          bool
	RequireAccountAuthentication bool
	CommercialMembership         CommercialMembershipService
	RequireVerifiedIdentity      bool
	BrowserClientFactory         func(sessionID, workspaceRoot string) *http.Client
}

func NewHandler(config Config, dependencies Dependencies) (http.Handler, error) {
	// Portable admission is part of the linked product, not an optional caller
	// flag or inherited environment setting. BYOK and local APIs still require
	// a verified Meadow account independently of paid capabilities.
	if buildinfo.DesktopPackageMode == "portable" && !buildinfo.VMLab {
		dependencies.RequireAccountAuthentication = true
	}
	if dependencies.RuntimeControl == nil {
		return nil, errors.New("daemon runtime control is required")
	}
	if dependencies.RequireVerifiedIdentity {
		if _, external := dependencies.RuntimeControl.(runtimecontrol.ExternalConnection); external {
			return nil, errors.New("protected native runtime cannot accept external API connection overrides")
		}
	}
	if dependencies.ModelSource == nil {
		return nil, errors.New("daemon model source is required")
	}
	if dependencies.RequireAccountAuthentication && dependencies.ProductAuth == nil {
		return nil, errors.New("daemon account authentication is required")
	}
	if dependencies.RequireVerifiedIdentity && dependencies.Browser == nil {
		return nil, errors.New("protected daemon browser owner is required")
	}
	if dependencies.RequireVerifiedIdentity && dependencies.ProductAuth == nil {
		return nil, errors.New("protected daemon product authorization is required")
	}
	if (dependencies.RequireVerifiedIdentity || dependencies.RequireAccountAuthentication) && dependencies.CommercialMembership == nil {
		return nil, errors.New("daemon commercial membership bridge is required")
	}
	if dependencies.RequireVerifiedIdentity && dependencies.ProductUpdate == nil {
		return nil, errors.New("protected daemon signed product-update service is required")
	}
	if dependencies.RequireVerifiedIdentity && dependencies.OfficialModels == nil {
		return nil, errors.New("protected daemon official model service is required")
	}
	if err := applyBrowserPipelinePolicy(dependencies.RequireVerifiedIdentity); err != nil {
		return nil, err
	}
	journal, err := newSessionJournal(config, dependencies.RequireVerifiedIdentity)
	if err != nil {
		return nil, err
	}
	var commercial *commercialBridge
	var requireAccount func(context.Context) error
	if dependencies.RequireAccountAuthentication {
		requireAccount = func(ctx context.Context) error {
			var status productauth.Status
			var err error
			if commercial != nil && commercial.promptGrants != nil {
				status, err = commercial.runtimeAccountStatus(ctx)
			} else {
				status, err = dependencies.ProductAuth.Status(ctx)
			}
			return AccountAuthenticationError(status, err)
		}
	}
	modelFactory := func(modelID string) agentcore.KernelModel {
		model, sourceErr := dependencies.ModelSource.Model(modelID)
		owned := &runtimeOwnedModel{
			modelID: strings.TrimSpace(modelID), runtimeControl: dependencies.RuntimeControl,
			delegate: model, sourceErr: sourceErr,
			requireVerifiedIdentity: dependencies.RequireVerifiedIdentity,
			requireAccount:          requireAccount,
		}
		if external, ok := dependencies.RuntimeControl.(externalConfigurationOwner); ok {
			identity := external.ConfigurationIdentity()
			owned.connectionIdentity = &identity
		}
		// A cold Core is not yet observable through RuntimeControl. Seed the
		// selected route's validated output capacity before the harness chooses
		// its default; matching live capacity still supersedes it on activation.
		// Read per factory call so a newly selected route sees catalog updates.
		if !dependencies.RequireVerifiedIdentity {
			if catalog, err := configuredUpstreamCatalog(); err == nil && catalog != nil {
				if profile, ok := catalog.Lookup(owned.modelID); ok {
					owned.SetOutputTokenLimit(profile.MaxOutput)
				}
			}
		}
		return owned
	}
	productHandler, err := producthost.NewHandler(config, producthost.Options{
		RuntimeControl: dependencies.RuntimeControl, NewProviderClient: modelFactory,
		TraceObserver:           dependencies.TraceObserver,
		BrowserClientFactory:    dependencies.BrowserClientFactory,
		RequireToolAccount:      requireAccount,
		BackgroundWakeAdmission: backgroundWakeAdmission(dependencies),
		ModelAllowed: func(modelID string) bool {
			if connection, ok := dependencies.RuntimeControl.(runtimecontrol.ExternalConnection); ok {
				return connection.AllowsModel(modelID)
			}
			allowed, _ := modelAdmissionByRuntimeProfile(modelID, dependencies.RequireVerifiedIdentity)
			return allowed
		},
	})
	if err != nil {
		return nil, err
	}
	configurationGate, ok := productHandler.(interface {
		WithConfigurationAdmission(http.Handler) http.Handler
		WithSessionConfigurationAdmission(http.Handler) http.Handler
		configurationLeaseApplier
	})
	if !ok {
		return nil, errors.New("product configuration admission boundary unavailable")
	}
	sessionHandler := http.Handler(productHandler)
	if journal != nil {
		sessionHandler = journal.Wrap(sessionHandler)
	}
	// The commercial wrapper resolves canonical conversation IDs to the
	// session owner's IDs. Check that resolved binding before journal writes.
	sessionHandler = configurationGate.WithSessionConfigurationAdmission(sessionHandler)
	commercial, err = newCommercialBridge(config.StateRoot, dependencies.CommercialMembership)
	if err != nil {
		return nil, err
	}
	if commercial != nil {
		// The commercial principal gate can own the account admission only when
		// both interfaces refer to the same native service instance and no paid
		// product guard precedes it. Other compositions retain their original
		// outer account proof and paid-admission ordering.
		commercial.accountAdmission = dependencies.RequireAccountAuthentication && dependencies.ProductAuthOptional &&
			sameNativeMembershipService(dependencies.ProductAuth, dependencies.CommercialMembership)
		if commercial.accountAdmission {
			commercial.enablePromptGrants()
		}
		sessionHandler = commercial.WrapSession(sessionHandler)
	}
	server := &serverHandler{
		runtimeControl:          dependencies.RuntimeControl,
		browser:                 dependencies.Browser,
		journal:                 journal,
		next:                    sessionHandler,
		requireVerifiedIdentity: dependencies.RequireVerifiedIdentity,
	}
	mux := http.NewServeMux()
	if buildinfo.VMLab {
		guest := func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, productauth.Status{Schema: productauth.SchemaV1, State: productauth.StateGuest, Plan: productauth.PlanGuest, Reason: "vm_lab_account_not_required"})
		}
		for _, route := range []string{"GET /auth/status", "GET /auth/admission-status", "GET /entitlement/status"} {
			mux.HandleFunc(route, guest)
		}
	}
	discovery := &connectionDiscovery{control: dependencies.RuntimeControl, source: dependencies.ModelSource, auth: dependencies.ProductAuth}
	mux.Handle("GET /agent/connections/models", configurationGate.WithConfigurationAdmission(http.HandlerFunc(discovery.handle)))
	mux.Handle("POST /agent/connections/reload", connectionReloadHandler(dependencies.RuntimeControl, configurationGate))
	if buildinfo.VMLab {
		mux.Handle("POST /agent/connections/inception", inceptionConfigurationHandler(dependencies.RuntimeControl, configurationGate))
	}
	mux.Handle("POST /agent/model", configurationGate.WithConfigurationAdmission(http.HandlerFunc(server.handleActivate)))
	mux.HandleFunc("GET /agent/model", server.handleStatus)
	mux.Handle("DELETE /agent/model", configurationGate.WithConfigurationAdmission(http.HandlerFunc(server.handleUnload)))
	mux.HandleFunc("GET /model/list", server.handleProductModels)
	if dependencies.Browser != nil {
		mux.Handle("POST /agent/browser/load", configurationGate.WithConfigurationAdmission(http.HandlerFunc(server.handleBrowserLoad)))
		mux.Handle("POST /agent/browser/inspect", configurationGate.WithConfigurationAdmission(http.HandlerFunc(server.handleBrowserInspect)))
		mux.HandleFunc("POST /agent/browser/cancel", server.handleBrowserCancel)
		mux.HandleFunc("POST /agent/browser/status", server.handleBrowserStatus)
	}
	if journal != nil {
		mux.HandleFunc("GET /agent/checkpoint/{session}", server.handleCheckpointRecovery)
	}
	if dependencies.ProductUpdate != nil {
		newUpdateRoutes(dependencies.ProductUpdate).register(mux)
	}
	if dependencies.OfficialModels != nil {
		newModelRoutes(dependencies.OfficialModels).register(mux)
	}
	if commercial != nil {
		commercial.register(mux)
	}
	var handler http.Handler = mux
	if dependencies.ProductAuth != nil {
		var revocationCallbacks []func()
		if !dependencies.ProductAuthOptional {
			revocationCallbacks = append(revocationCallbacks, func() { _ = dependencies.RuntimeControl.Close() })
		}
		authRoutes := newProductAuthRoutes(dependencies.ProductAuth, revocationCallbacks...)
		if commercial != nil && sameNativeMembershipService(dependencies.ProductAuth, dependencies.CommercialMembership) {
			authRoutes.onStatus = commercial.resumeConsumption
			if commercial.promptGrants != nil {
				authRoutes.accountStatus = commercial.admissionStatus
				commercial.configureRuntimeAccountReaders(authRoutes, discovery)
			}
		}
		authRoutes.register(mux)
		if !dependencies.ProductAuthOptional {
			handler = authRoutes.protect(mux)
		}
		if dependencies.RequireAccountAuthentication {
			if commercial != nil && commercial.accountAdmission {
				handler = authRoutes.protectAccountDelegating(handler, commercial.ownsAccountAdmission)
			} else {
				handler = authRoutes.protectAccount(handler)
			}
		}
	}
	mux.Handle("/", configurationGate.WithSessionConfigurationAdmission(sessionHandler))
	bounded := newBoundedHandler(handler)
	bounded = &ownedSessionHandler{Handler: bounded, owner: productHandler.(interface{ Close() error })}
	if external, ok := dependencies.RuntimeControl.(externalConfigurationOwner); ok {
		return &connectionIdentityHandler{Handler: bounded, owner: external}, nil
	}
	return bounded, nil
}

type runtimeOwnedModel struct {
	modelID                 string
	runtimeControl          runtimecontrol.RuntimeControl
	delegate                agentcore.KernelModel
	sourceErr               error
	requireVerifiedIdentity bool
	requireAccount          func(context.Context) error
	connectionIdentity      *runtimecontrol.ExternalConfiguration
}

// Forward capacity through the ownership wrapper so both session metadata and
// the final provider request use the capability advertised by Meadow Core.
func (model *runtimeOwnedModel) SetOutputTokenLimit(limit int) {
	if model == nil {
		return
	}
	if configurable, ok := model.delegate.(interface{ SetOutputTokenLimit(int) }); ok {
		configurable.SetOutputTokenLimit(limit)
	}
}

func (model *runtimeOwnedModel) OutputTokenLimit() int {
	if model != nil {
		if limits, ok := model.delegate.(interface{ OutputTokenLimit() int }); ok {
			return limits.OutputTokenLimit()
		}
	}
	return 0
}

func (model *runtimeOwnedModel) ModelCapabilities() (provider.ModelProfile, bool) {
	if model == nil {
		return provider.ModelProfile{}, false
	}
	return provider.ProfileForClient(model.delegate, model.modelID)
}

func (model *runtimeOwnedModel) ChatStream(ctx context.Context, request provider.ChatRequest, hooks *provider.StreamHooks) (*provider.StreamResult, error) {
	if model == nil || model.runtimeControl == nil {
		return nil, errors.New("daemon inference ownership is unavailable")
	}
	if model.requireAccount != nil {
		if err := model.requireAccount(ctx); err != nil {
			return nil, err
		}
	}
	if model.connectionIdentity != nil {
		current, ok := model.runtimeControl.(externalConfigurationOwner)
		if !ok {
			return nil, errors.New("API connection binding is unavailable")
		}
		identity := current.ConfigurationIdentity()
		bound := model.connectionIdentity
		if identity.ConnectionID != bound.ConnectionID || identity.Fingerprint != bound.Fingerprint || identity.Generation != bound.Generation {
			return nil, errors.New("API configuration changed; reopen the chat under its original connection")
		}
	}
	allowed, configured := modelAdmissionByRuntimeProfile(model.modelID, model.requireVerifiedIdentity)
	external, isExternal := model.runtimeControl.(runtimecontrol.ExternalConnection)
	if isExternal {
		allowed, configured = external.AllowsModel(model.modelID), true
	}
	if !allowed {
		return nil, fmt.Errorf("model %q is not available in the current product catalog", model.modelID)
	}
	residentMatches := func(resident string) bool {
		if configured {
			return resident == model.modelID
		}
		return strings.EqualFold(resident, model.modelID)
	}
	if model.sourceErr != nil {
		return nil, model.sourceErr
	}
	if model.delegate == nil {
		return nil, errors.New("daemon model source returned no inference provider")
	}
	requested := strings.TrimSpace(request.Model)
	if requested != "" && requested != model.modelID {
		return nil, fmt.Errorf("inference model %q does not match daemon binding %q", requested, model.modelID)
	}
	if isExternal {
		connection, err := external.Connection()
		if err != nil {
			return nil, err
		}
		capability, ok := connection.LookupModel(model.modelID)
		if !ok {
			return nil, errors.New("API model binding is unavailable")
		}
		model.SetOutputTokenLimit(capability.MaxOutputTokens)
		request.Model = model.modelID
		return model.delegate.ChatStream(ctx, request, hooks)
	}
	status, ready, err := model.runtimeControl.Status(ctx)
	resident, residentReady := status.ResidentModel()
	if err != nil || !ready || !residentReady || !residentMatches(resident) {
		if _, activateErr := model.runtimeControl.Activate(ctx, model.modelID); activateErr != nil {
			return nil, fmt.Errorf("activate daemon-owned model %q: %w", model.modelID, activateErr)
		}
		status, ready, err = model.runtimeControl.Status(ctx)
		if err != nil {
			return nil, fmt.Errorf("read daemon-owned runtime status: %w", err)
		}
	}
	resident, residentReady = status.ResidentModel()
	if !ready || !residentReady || !residentMatches(resident) {
		return nil, fmt.Errorf("daemon-owned model %q is not ready", model.modelID)
	}
	if model.requireVerifiedIdentity {
		if err := status.VerifyRuntimeIdentity(model.modelID); err != nil {
			return nil, fmt.Errorf("verify daemon-owned model identity: %w", err)
		}
	}
	// Status was already checked for ownership above; reuse that observation
	// at every request boundary, including a same-model backend reconfiguration.
	model.SetOutputTokenLimit(status.MaxOutputTokens)
	request.Model = model.modelID
	return model.delegate.ChatStream(ctx, request, hooks)
}

func (model *runtimeOwnedModel) MaintainActivityLease(ctx context.Context, modelID string) {
	if model == nil || model.delegate == nil {
		return
	}
	if leaser, ok := model.delegate.(interface{ MaintainActivityLease(context.Context, string) }); ok {
		leaser.MaintainActivityLease(ctx, modelID)
	}
}

type serverHandler struct {
	runtimeControl          runtimecontrol.RuntimeControl
	browser                 BrowserOwner
	journal                 *sessionJournal
	next                    http.Handler
	requireVerifiedIdentity bool
}

func (server *serverHandler) handleActivate(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeJSON(request, &body, true); err != nil || strings.TrimSpace(body.Model) == "" {
		writeError(writer, http.StatusBadRequest, "model is required")
		return
	}
	requested := strings.TrimSpace(body.Model)
	allowed := modelAllowedByRuntimeProfile(requested, server.requireVerifiedIdentity)
	if external, ok := server.runtimeControl.(runtimecontrol.ExternalConnection); ok {
		allowed = external.AllowsModel(requested)
	}
	if !allowed {
		writeError(writer, http.StatusForbidden, "model is not available in the current product catalog")
		return
	}
	result, err := server.runtimeControl.Activate(request.Context(), requested)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, err.Error())
		return
	}
	if _, external := server.runtimeControl.(runtimecontrol.ExternalConnection); external {
		// API activation only changes the configured route. Return its actual
		// observation inside this already-authorized configuration transaction,
		// so presentation clients need not authorize a second status request.
		// This does not probe the provider or claim its inference service is up.
		status, ready, statusErr := server.runtimeControl.Status(request.Context())
		if statusErr != nil || !ready || !result.Accepted || strings.TrimSpace(result.Model) != requested ||
			strings.TrimSpace(status.Model.ID) != requested || !externalModelState(result.State) || !externalModelState(status.Model.State) {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": "API model selection could not be confirmed"})
			return
		}
		writeJSON(writer, http.StatusOK, struct {
			runtimecontrol.LifecycleResult
			Ready  bool                  `json:"ready"`
			Status runtimecontrol.Status `json:"status"`
		}{LifecycleResult: result, Ready: true, Status: status})
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func externalModelState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "configured", "ready", "loaded":
		return true
	default:
		return false
	}
}

func (server *serverHandler) handleUnload(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeJSON(request, &body, false); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid model unload request")
		return
	}
	result, err := server.runtimeControl.Unload(request.Context(), strings.TrimSpace(body.Model))
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *serverHandler) handleStatus(writer http.ResponseWriter, request *http.Request) {
	var configurationOwner externalConfigurationOwner
	var configurationBefore runtimecontrol.ExternalConfiguration
	if request.URL.Query().Get("catalog") == "1" {
		if _, external := server.runtimeControl.(runtimecontrol.ExternalConnection); external {
			configurationOwner, _ = server.runtimeControl.(externalConfigurationOwner)
			if configurationOwner != nil {
				configurationBefore = configurationOwner.ConfigurationIdentity()
			}
		}
	}
	status, ready, err := server.runtimeControl.Status(request.Context())
	var modelRows []ProductModelEntry
	includeCatalog := false
	if external, ok := server.runtimeControl.(runtimecontrol.ExternalConnection); ok && request.URL.Query().Get("catalog") == "1" {
		includeCatalog = true
		var catalogErr error
		modelRows, catalogErr = externalProductModels(external)
		if catalogErr != nil {
			ready = false
		} else if configurationOwner != nil && configurationOwner.ConfigurationIdentity() != configurationBefore {
			// Model ID alone cannot pin a revision: a settings reload may keep
			// that ID while changing provider identity or capacity metadata.
			ready = false
		} else if ready {
			// Do not combine observations from different selections into a ready
			// receipt. A concurrent configuration change can be retried normally.
			matches := false
			for _, row := range modelRows {
				if row.Active && row.ModelID == strings.TrimSpace(status.Model.ID) {
					matches = true
					break
				}
			}
			if !matches {
				ready = false
			}
		}
	}
	if err != nil || !ready {
		message := "Meadow Runtime is not ready"
		if err != nil {
			message = err.Error()
		}
		result := map[string]any{"ready": false, "error": message}
		if includeCatalog {
			// Configured choices remain visible even while route status is not
			// ready; catalog presence is not evidence of provider availability.
			result["modelRows"] = modelRows
		}
		writeJSON(writer, http.StatusServiceUnavailable, result)
		return
	}
	result := map[string]any{"ready": true, "status": status}
	if includeCatalog {
		result["modelRows"] = modelRows
	}
	writeJSON(writer, http.StatusOK, result)
}

// ProductModelEntry is the Desktop picker contract. It is intentionally
// separate from the signed Model Vault catalog: the picker describes product
// identities already delivered with the configured edition, while Model Vault
// remains a fail-closed install/update surface for signed artifacts.
type ProductModelEntry struct {
	Name                 string `json:"name"`
	ModelID              string `json:"modelId"`
	Delivery             string `json:"delivery,omitempty"`
	Description          string `json:"description,omitempty"`
	ProductRole          string `json:"productRole,omitempty"`
	RoleLabel            string `json:"roleLabel,omitempty"`
	RoleDescription      string `json:"roleDescription,omitempty"`
	ProductionReady      bool   `json:"productionReady"`
	HarnessStatus        string `json:"harnessStatus,omitempty"`
	RecommendedMemoryGiB int64  `json:"recommendedMemoryGiB,omitempty"`
	Multimodal           bool   `json:"multimodal"`
	Installed            bool   `json:"installed"`
	Present              bool   `json:"present"`
	Live                 bool   `json:"live"`
	Active               bool   `json:"active"`
}

func (server *serverHandler) handleProductModels(writer http.ResponseWriter, request *http.Request) {
	if external, ok := server.runtimeControl.(runtimecontrol.ExternalConnection); ok {
		rows, _ := externalProductModels(external)
		writeJSON(writer, http.StatusOK, rows)
		return
	}
	profile := edition.ReadOverride()
	if profile == "" {
		profile = edition.Standard
	}
	status, ready, err := server.runtimeControl.Status(request.Context())
	if err != nil {
		// Runtime readiness must not erase the configured product catalog. The
		// model picker remains usable during cold startup and reports no live row.
		ready = false
		status = runtimecontrol.Status{}
	}
	if !server.requireVerifiedIdentity {
		catalog, catalogErr := configuredUpstreamCatalog()
		if catalogErr != nil {
			// An invalid explicit catalog must not silently restore the local
			// product allowlist or expose parser paths/provider configuration.
			writeError(writer, http.StatusServiceUnavailable, "configured model catalog is unavailable")
			return
		}
		ids := configuredUpstreamModelIDs()
		if catalog != nil {
			ids = catalog.ModelIDs()
		}
		if len(ids) > 0 {
			resident, residentReady := status.ResidentModel()
			rows := []ProductModelEntry{}
			for _, modelID := range ids {
				live := ready && residentReady && resident == modelID
				name, description := modelID, "模型 API · via Meadow Core"
				if catalog != nil {
					profile, _ := catalog.Lookup(modelID)
					if profile.DisplayName != "" {
						name = profile.DisplayName
					}
					if profile.Provider != "" {
						description = profile.Provider + " · 模型 API · via Meadow Core"
					}
				}
				rows = append(rows, ProductModelEntry{
					Name: name, ModelID: modelID, Delivery: "api",
					Description: description, RoleLabel: "API 比較模式", HarnessStatus: "configured",
					Present: live, Live: live, Active: live,
					Multimodal: live && status.ModelIdentity.CanonicalID == modelID && status.ModelIdentity.Multimodal,
				})
			}
			writeJSON(writer, http.StatusOK, rows)
			return
		}
	}
	writeJSON(writer, http.StatusOK, ProductModelCatalog(profile, status, ready))
}

func externalProductModels(external runtimecontrol.ExternalConnection) ([]ProductModelEntry, error) {
	connection, err := external.Connection()
	if err != nil {
		return []ProductModelEntry{}, err
	}
	selected, _, _ := external.ConfiguredModel()
	rows := make([]ProductModelEntry, 0, len(connection.Models))
	for _, model := range connection.Models {
		if model.Disabled {
			continue
		}
		name := model.DisplayName
		if name == "" {
			name = model.ID
		}
		rows = append(rows, ProductModelEntry{Name: name, ModelID: model.ID, Delivery: "api", Description: connection.Name + " · API", HarnessStatus: "configured", Active: model.ID == selected, Multimodal: model.SupportsImages})
	}
	return rows, nil
}

// Development upstream profiles admit only their explicit configured catalog. They
// do not expand the signed product catalog or its entitlement vocabulary.
func modelAllowedByRuntimeProfile(modelID string, requireVerifiedIdentity bool) bool {
	allowed, _ := modelAdmissionByRuntimeProfile(modelID, requireVerifiedIdentity)
	return allowed
}

func modelAdmissionByRuntimeProfile(modelID string, requireVerifiedIdentity bool) (allowed, configured bool) {
	if !requireVerifiedIdentity {
		catalog, err := configuredUpstreamCatalog()
		if err != nil {
			return false, true
		}
		if catalog != nil {
			_, allowed = catalog.Lookup(strings.TrimSpace(modelID))
			return allowed, true
		}
		if configuredUpstreamModelID() != "" {
			return configuredUpstreamModelAllowed(modelID), true
		}
	}
	return modelmgr.IsCurrentProductModel(modelID), false
}

// ProductModelCatalog returns the edition-aware picker rows without touching
// the signed artifact/download catalog. Protected startup uses this pure view
// while a cold runtime is still unavailable.
func ProductModelCatalog(profile string, status runtimecontrol.Status, runtimeReady bool) []ProductModelEntry {
	resident, residentReady := status.ResidentModel()
	residentReady = residentReady && runtimeReady
	entry := func(info modelmgr.ModelInfo) ProductModelEntry {
		position, _ := modelmgr.ProductPositionFor(info.ServedName)
		harnessStatus := "adapting"
		if position.Ready {
			harnessStatus = "ready"
		}
		live := residentReady && strings.EqualFold(strings.TrimSpace(resident), info.ServedName)
		profile, profileKnown := provider.ProfileForModel(info.ServedName)
		return ProductModelEntry{
			Name: info.ServedName, ModelID: info.ServedName,
			ProductRole: string(position.Role), RoleLabel: position.Label,
			RoleDescription: position.CoreTasks, ProductionReady: position.Ready,
			HarnessStatus: harnessStatus, RecommendedMemoryGiB: info.MinRAMGiB,
			Multimodal: profileKnown && profile.Multimodal,
			Installed:  true, Present: true, Live: live, Active: live,
		}
	}

	byID := make(map[string]modelmgr.ModelInfo, 3)
	for _, info := range modelmgr.KnownModels() {
		byID[info.ServedName] = info
	}
	info, exists := byID["Meadow Coder 35B"]
	if !exists {
		return nil
	}
	if normalized, ok := edition.Normalize(profile); ok && normalized == edition.Skull {
		row := entry(info)
		row.Name = edition.SkullModelDisplayName
		row.Description = "Huihui Ornith 1.5 35B A3B · Abliterated"
		return []ProductModelEntry{row}
	}
	return []ProductModelEntry{entry(info)}
}

func decodeJSON(request *http.Request, target any, required bool) error {
	if request.Body == nil || request.ContentLength == 0 {
		if required {
			return errors.New("request body is required")
		}
		return nil
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("request contains more than one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("request contains invalid trailing data: %w", err)
	}
	return nil
}

type boundedHandler struct {
	next      http.Handler
	semaphore chan struct{}
}

func newBoundedHandler(next http.Handler) http.Handler {
	return &boundedHandler{next: next, semaphore: make(chan struct{}, MaxActiveRequests)}
}

func (handler *boundedHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	select {
	case handler.semaphore <- struct{}{}:
		defer func() { <-handler.semaphore }()
	default:
		writeError(writer, http.StatusServiceUnavailable, "Meadow Agent request capacity is exhausted")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, MaxRequestBytes)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)
	bounded := &boundedResponseWriter{ResponseWriter: writer, remaining: MaxEventBytes, cancel: cancel}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	handler.next.ServeHTTP(bounded, request)
}

type boundedResponseWriter struct {
	http.ResponseWriter
	remaining int64
	cancel    context.CancelFunc
}

func (writer *boundedResponseWriter) Write(payload []byte) (int, error) {
	if int64(len(payload)) > writer.remaining {
		writer.cancel()
		return 0, errEventLimit
	}
	written, err := writer.ResponseWriter.Write(payload)
	writer.remaining -= int64(written)
	return written, err
}

func (writer *boundedResponseWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *boundedResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": message, "status": status})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
