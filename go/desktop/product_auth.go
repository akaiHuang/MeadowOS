package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/agentdaemon"
	licenseproductauth "github.com/hey-meadow/meadow-code/go/core/licenseclient/productauth"
	coreauth "github.com/hey-meadow/meadow-code/go/core/productauth"
)

type desktopProductAuth interface {
	Status(context.Context) (coreauth.Status, error)
	Start(context.Context, coreauth.LoginMode) (coreauth.StartResult, error)
	Poll(context.Context, string) (coreauth.PollResult, error)
	Refresh(context.Context) (coreauth.Status, error)
	Logout(context.Context) error
	Redeem(context.Context, string) (coreauth.RedeemResult, error)
	RedeemRequest(context.Context, coreauth.RedeemRequest) (coreauth.RedeemResult, error)
	ListDevices(context.Context) (coreauth.DeviceListResult, error)
	RevokeDeviceRequest(context.Context, coreauth.DeviceRevokeRequest) (coreauth.DeviceRevokeResult, error)
}

func registerDesktopProductAuthRoutes(api *http.ServeMux, client desktopProductAuth, onAuthorized func()) {
	registerDesktopProductAuthRoutesWithLifecycle(api, client, onAuthorized, nil)
}

func registerDesktopProductAuthRoutesWithLifecycle(api *http.ServeMux, client desktopProductAuth, onAuthorized, onUnauthorized func(), loginURLs ...*desktopLoginURLRegistry) {
	var loginURL *desktopLoginURLRegistry
	if len(loginURLs) == 1 {
		loginURL = loginURLs[0]
	}
	notify := func(status coreauth.Status, err error) {
		if err != nil {
			return
		}
		if desktopStatusAuthorized(status) {
			if onAuthorized != nil {
				go onAuthorized()
			}
		} else if onUnauthorized != nil {
			go onUnauthorized()
		}
	}
	statusHandler := func(w http.ResponseWriter, r *http.Request) {
		status, err := client.Status(r.Context())
		notify(status, err)
		writeDesktopProductAuthResult(w, status, err)
	}
	api.HandleFunc("GET /license/status", statusHandler)
	admissionHandler := func(w http.ResponseWriter, r *http.Request) {
		status, err := desktopAccountStatus(r.Context(), client)
		notify(status, err)
		writeDesktopProductAuthResult(w, status, err)
	}
	api.HandleFunc("GET /auth/status", admissionHandler)
	api.HandleFunc("GET /auth/admission-status", admissionHandler)
	startHandler := func(w http.ResponseWriter, r *http.Request) {
		var body coreauth.StartRequest
		if decodeDesktopAuthJSON(r, &body) != nil {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		if body.Schema != coreauth.SchemaV1 {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		var generation uint64
		if loginURL != nil {
			generation = loginURL.reset()
		}
		result, err := client.Start(r.Context(), body.Mode)
		if err == nil && r.Context().Err() == nil && loginURL != nil {
			loginURL.remember(generation, result)
		}
		writeDesktopProductAuthResult(w, result, err)
	}
	api.HandleFunc("POST /license/login/start", startHandler)
	api.HandleFunc("POST /auth/start", startHandler)
	pollHandler := func(w http.ResponseWriter, r *http.Request) {
		var body coreauth.PollRequest
		if decodeDesktopAuthJSON(r, &body) != nil {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		if body.Schema != coreauth.SchemaV1 {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		transactionID := strings.TrimSpace(body.TransactionID)
		result, err := client.Poll(r.Context(), transactionID)
		if err == nil && loginURL != nil && result.State != coreauth.StateLoginPending {
			loginURL.complete(transactionID)
		}
		if result.Status != nil {
			notify(*result.Status, err)
		}
		writeDesktopProductAuthResult(w, result, err)
	}
	api.HandleFunc("POST /license/login/poll", pollHandler)
	api.HandleFunc("POST /auth/poll", pollHandler)
	api.HandleFunc("POST /license/redeem", func(w http.ResponseWriter, r *http.Request) {
		var body coreauth.RedeemRequest
		if decodeDesktopAuthJSON(r, &body) != nil {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		result, err := client.RedeemRequest(r.Context(), body)
		notify(result.Availability, err)
		writeDesktopProductAuthResult(w, result, err)
	})
	api.HandleFunc("POST /license/activate-key", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LicenseKey string `json:"license_key"`
		}
		if decodeDesktopAuthJSON(r, &body) != nil {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		result, err := client.Redeem(r.Context(), body.LicenseKey)
		notify(result.Availability, err)
		writeDesktopProductAuthResult(w, result, err)
	})
	refresh := func(w http.ResponseWriter, r *http.Request) {
		var body coreauth.RefreshRequest
		if decodeDesktopAuthJSON(r, &body) != nil || body.Schema != coreauth.SchemaV1 {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		status, err := client.Refresh(r.Context())
		notify(status, err)
		writeDesktopProductAuthResult(w, status, err)
	}
	api.HandleFunc("POST /auth/refresh", refresh)
	logoutHandler := func(w http.ResponseWriter, r *http.Request) {
		var body coreauth.LogoutRequest
		if decodeDesktopAuthJSON(r, &body) != nil || body.Schema != coreauth.SchemaV1 {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		if loginURL != nil {
			loginURL.reset()
		}
		err := client.Logout(r.Context())
		if err == nil && onUnauthorized != nil {
			go onUnauthorized()
		}
		writeDesktopProductAuthResult(w, map[string]any{"schema": coreauth.SchemaV1, "ok": err == nil}, err)
	}
	api.HandleFunc("POST /auth/logout", logoutHandler)
	api.HandleFunc("POST /devices/list", func(w http.ResponseWriter, r *http.Request) {
		var body coreauth.DeviceListRequest
		if decodeDesktopAuthJSON(r, &body) != nil || body.Schema != coreauth.SchemaV1 {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		result, err := client.ListDevices(r.Context())
		writeDesktopProductAuthResult(w, result, err)
	})
	api.HandleFunc("POST /devices/revoke", func(w http.ResponseWriter, r *http.Request) {
		var body coreauth.DeviceRevokeRequest
		if decodeDesktopAuthJSON(r, &body) != nil || body.Schema != coreauth.SchemaV1 {
			writeDesktopProductAuthBadRequest(w)
			return
		}
		result, err := client.RevokeDeviceRequest(r.Context(), body)
		notify(result.Availability, err)
		writeDesktopProductAuthResult(w, result, err)
	})
}

func decodeDesktopAuthJSON(r *http.Request, output any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func writeDesktopProductAuthBadRequest(w http.ResponseWriter) {
	writeDesktopProductAuthResult(w, nil, &coreauth.Error{Code: "invalid_request", Message: "authorization request is invalid", HTTPStatus: http.StatusBadRequest})
}

func writeDesktopProductAuthResult(w http.ResponseWriter, output any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(licenseproductauth.ErrorStatus(err))
		_ = json.NewEncoder(w).Encode(licenseproductauth.PublicError(err))
		return
	}
	_ = json.NewEncoder(w).Encode(output)
}

func desktopProductAuthorized(ctx context.Context, client desktopProductAuth) bool {
	_, err := desktopAccountStatus(ctx, client)
	return err == nil
}

func desktopStatusAuthorized(status coreauth.Status) bool {
	return agentdaemon.AccountAuthenticationError(status, nil) == nil
}
