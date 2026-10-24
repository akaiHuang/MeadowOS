//go:build !meadow_protected

package main

import (
	"log"
	"strings"

	modelmgr "github.com/hey-meadow/meadow-code/go/core/model"
	"github.com/hey-meadow/meadow-code/go/core/session"
)

func preferredReadyModel(requested, reported string) string {
	if info, ok := modelmgr.ModelByName(strings.TrimSpace(requested)); ok {
		return info.ServedName
	}
	if reported = strings.TrimSpace(reported); reported != "" {
		return reported
	}
	return strings.TrimSpace(requested)
}

// persistPreferredModel records only a model that the supervisor has already
// brought to ready. A failed or merely requested model must never replace the
// user's last known-good selection.
func persistPreferredModel(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	settings, err := session.LoadSettings()
	if err != nil {
		log.Printf("persist ready model %q: load settings: %v", model, err)
		return
	}
	if err := settings.SetDefaultModel(model); err != nil {
		log.Printf("persist ready model %q: %v", model, err)
	}
}
