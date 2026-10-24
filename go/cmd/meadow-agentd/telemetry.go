package main

import (
	"context"
	"errors"
	"github.com/hey-meadow/meadow-code/go/core/productauth"
	"github.com/hey-meadow/meadow-code/go/core/telemetry"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type authenticatedTelemetry interface {
	telemetry.Exporter
	TelemetryPrincipal() string
}

// Full Langfuse infrastructure and platform credentials remain on the server.
// Desktop sends only to the fixed, signed product-auth receiver.
func configureTelemetry(config startupConfig, composition *startupComposition) {
	if composition == nil || strings.TrimSpace(os.Getenv("MEADOW_TELEMETRY_DISABLED")) == "1" {
		return
	}
	auth, ok := composition.productAuth.(authenticatedTelemetry)
	if !ok {
		return
	}
	root := strings.TrimSpace(config.agent.StateRoot)
	if root == "" {
		return
	}
	queue, err := telemetry.New(telemetry.Config{Directory: filepath.Join(root, "telemetry-outbox"), Exporter: auth, Principal: auth.TelemetryPrincipal, Diagnostic: strings.TrimSpace(os.Getenv("MEADOW_TELEMETRY_DIAGNOSTIC")) == "1", ClassifyFailure: classifyTelemetryFailure})
	if err != nil {
		log.Print("Meadow background telemetry unavailable")
		return
	}
	composition.telemetry = queue
	go reportTelemetryStateChanges(queue, func(message string) { log.Print(message) })
}

func classifyTelemetryFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var remote *productauth.Error
	if !errors.As(err, &remote) {
		return "unknown"
	}
	// These locally generated errors use HTTP-shaped status codes even when
	// no response arrived. Do not misdiagnose transport or secure-store failure.
	switch remote.Code {
	case "network_unavailable":
		return "transport"
	case "keychain_unavailable":
		return "credentials_unavailable"
	case "invalid_server_response":
		return "invalid_response"
	case "auth_unavailable":
		return "authentication_unavailable"
	}
	if code := telemetry.SanitizeFailureCode(remote.Code); code != "unknown" {
		return code
	}
	switch {
	case remote.HTTPStatus == 401:
		return "http_unauthorized"
	case remote.HTTPStatus == 403:
		return "http_forbidden"
	case remote.HTTPStatus == 429:
		return "http_rate_limited"
	case remote.HTTPStatus >= 500 && remote.HTTPStatus <= 599:
		return "http_server_error"
	case remote.HTTPStatus >= 400 && remote.HTTPStatus <= 499:
		return "http_client_error"
	case remote.HTTPStatus == 0:
		return "transport"
	default:
		return "unknown"
	}
}

func reportTelemetryStateChanges(queue *telemetry.Queue, report func(string)) {
	for change := range queue.ExportStateChanges() {
		if !queue.IsCurrentExportState(change) {
			continue
		}
		if change.Recovered {
			report("Meadow telemetry export recovered")
		} else {
			report("Meadow telemetry export failed: " + telemetry.SanitizeFailureCode(change.FailureCode))
		}
	}
}
func closeTelemetry(queue *telemetry.Queue) {
	if queue == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = queue.Close(ctx)
}
