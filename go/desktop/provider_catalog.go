//go:build !meadow_protected

package main

import (
	"net/http"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

func registerProviderCatalogHandlers(api *http.ServeMux) {
	api.HandleFunc(
		"GET /native/provider-catalog",
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(
				w,
				map[string]any{"providers": provider.ProviderCatalog()},
			)
		},
	)
}
