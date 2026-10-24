package telemetry

// ExportStateChange contains only a fixed classification. The originating
// native session is private and rechecked by consumers before logging.
type ExportStateChange struct {
	FailureCode string
	Recovered   bool
	principal   string
}

type exportFailure struct{ principal, code string }

func SanitizeFailureCode(code string) string {
	switch code {
	case "invalid_device_proof", "invalid_access_token", "access_token_expired",
		"session_revoked", "device_revoked", "account_suspended", "authentication_required",
		"telemetry_capacity", "telemetry_unavailable", "telemetry_rejected", "telemetry_disabled", "invalid_telemetry",
		"http_unauthorized", "http_forbidden", "http_rate_limited", "http_client_error", "http_server_error",
		"deadline_exceeded", "canceled", "transport", "credentials_unavailable", "invalid_response", "authentication_unavailable":
		return code
	default:
		return "unknown"
	}
}

func (q *Queue) ExportStateChanges() <-chan ExportStateChange {
	if q == nil {
		return nil
	}
	return q.stateChanges
}

func (q *Queue) IsCurrentExportState(change ExportStateChange) bool {
	return q != nil && change.principal != "" && change.principal == q.Principal()
}

// Called only by the queue worker. Reporting cannot block persistence, retry,
// model emission, or shutdown, even when no log consumer is available.
func (q *Queue) recordExportState(principal string, err error) {
	if principal == "" || principal != q.Principal() {
		return
	}
	previous := q.failure.Load()
	previousCode := ""
	if previous != nil && previous.principal == principal {
		previousCode = previous.code
	}
	code := ""
	if err != nil {
		code = "unknown"
		if q.config.ClassifyFailure != nil {
			code = SanitizeFailureCode(q.config.ClassifyFailure(err))
		}
	}
	q.failure.Store(&exportFailure{principal: principal, code: code})
	if code == previousCode {
		return
	}
	change := ExportStateChange{FailureCode: code, Recovered: code == "", principal: principal}
	select {
	case q.stateChanges <- change:
	default:
		q.statusChangesDropped.Add(1)
	}
}
