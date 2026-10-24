package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type meadowBrowserDiagnostic struct {
	ID        int               `json:"id"`
	PageID    string            `json:"page_id"`
	Kind      string            `json:"kind"`
	Level     string            `json:"level"`
	Message   string            `json:"message"`
	Text      string            `json:"text"`
	Timestamp string            `json:"ts"`
	SourceURL string            `json:"source_url"`
	Line      int               `json:"line"`
	Column    int               `json:"column"`
	Details   map[string]string `json:"details"`
}

type meadowBrowserDiagnosticsResponse struct {
	OK              bool                      `json:"ok"`
	ProtocolVersion string                    `json:"protocol_version"`
	Entries         []meadowBrowserDiagnostic `json:"entries"`
	ErrorCount      int                       `json:"error_count"`
	WarningCount    int                       `json:"warning_count"`
	LastID          int                       `json:"last_id"`
}

// browserDiagnostics reads Meadow Browser's structured diagnostics contract.
// The console-logs fallback keeps older daemons useful while upgrades roll
// out, but only the diagnostics.v1 endpoint can report resource, promise,
// navigation, and WebKit process failures with source coordinates.
func (t *MeadowBrowserVisualCheck) browserDiagnostics(
	ctx context.Context,
	base string,
) (meadowBrowserDiagnosticsResponse, error) {
	pageID, _ := t.browserPageID(ctx, base)
	query := url.Values{"limit": {"200"}, "level": {"warn"}}
	if pageID != "" {
		query.Set("page_id", pageID)
	}
	endpoint := base + "/diagnostics?" + query.Encode()
	var response meadowBrowserDiagnosticsResponse
	status, err := t.getJSON(ctx, endpoint, &response)
	if err == nil {
		return response, nil
	}
	if status != http.StatusNotFound {
		return response, fmt.Errorf("Meadow Browser diagnostics: %w", err)
	}

	legacyQuery := url.Values{"limit": {"200"}}
	if pageID != "" {
		legacyQuery.Set("page_id", pageID)
	}
	status, err = t.getJSON(ctx, base+"/console-logs?"+legacyQuery.Encode(), &response)
	if err != nil {
		if status == http.StatusNotFound {
			return response, fmt.Errorf("Meadow Browser does not expose /diagnostics or /console-logs; update the daemon before accepting browser evidence")
		}
		return response, fmt.Errorf("Meadow Browser console diagnostics: %w", err)
	}
	response.ProtocolVersion = "meadow.browser.console-legacy"
	response.ErrorCount = 0
	response.WarningCount = 0
	for index := range response.Entries {
		entry := &response.Entries[index]
		// Legacy daemons do not provide event IDs, so the old implementation
		// could not apply the pre-navigation cursor and treated days-old console
		// entries as current-page failures. Console logs are append ordered;
		// synthesize a stable cursor so only entries added after /goto survive.
		if entry.ID <= 0 {
			entry.ID = index + 1
		}
		if entry.ID > response.LastID {
			response.LastID = entry.ID
		}
		entry.Kind = "console"
		if entry.Message == "" {
			entry.Message = entry.Text
		}
		if entry.Level == "error" {
			response.ErrorCount++
		} else if entry.Level == "warn" {
			response.WarningCount++
		}
	}
	return response, nil
}

func (t *MeadowBrowserVisualCheck) browserPageID(ctx context.Context, base string) (string, error) {
	var state struct {
		PageID string `json:"page_id"`
	}
	status, err := t.getJSON(ctx, base+"/state", &state)
	if err != nil {
		if status == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(state.PageID), nil
}

func (t *MeadowBrowserVisualCheck) getJSON(ctx context.Context, endpoint string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := t.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) > 0 && out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (r meadowBrowserDiagnosticsResponse) errors() []meadowBrowserDiagnostic {
	seen := make(map[string]bool)
	var errors []meadowBrowserDiagnostic
	for _, entry := range r.Entries {
		if strings.ToLower(strings.TrimSpace(entry.Level)) != "error" {
			continue
		}
		if meadowBrowserInfrastructureNoise(entry) {
			continue
		}
		message := strings.TrimSpace(entry.Message)
		if message == "" {
			message = strings.TrimSpace(entry.Text)
		}
		entry.Message = message
		key := strings.Join([]string{entry.Kind, message, entry.SourceURL, strconv.Itoa(entry.Line), strconv.Itoa(entry.Column)}, "\x00")
		if message == "" || seen[key] {
			continue
		}
		seen[key] = true
		errors = append(errors, entry)
	}
	return errors
}

// meadowBrowserInfrastructureNoise removes a narrowly identified WebKit-side
// attestation request that is emitted by Meadow Browser itself. It is not
// initiated by the rendered local artifact and therefore cannot be repaired by
// editing that artifact. Keep this allow-list deliberately exact: ordinary
// resource failures, page JavaScript errors, and every other network host must
// remain visible to the validator.
func meadowBrowserInfrastructureNoise(entry meadowBrowserDiagnostic) bool {
	if strings.ToLower(strings.TrimSpace(entry.Kind)) != "network_error" {
		return false
	}
	rawURL := strings.TrimSpace(entry.SourceURL)
	if rawURL == "" {
		rawURL = strings.TrimSpace(entry.Message)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "jnn-pa.googleapis.com") {
		return false
	}
	return strings.HasPrefix(parsed.EscapedPath(), "/$rpc/google.internal.waa.v1.Waa/GenerateIT")
}

// diagnosticsSince isolates one validator navigation from errors already
// recorded in the same long-lived Meadow Browser page. Clearing the daemon's
// global buffer would race with concurrent Desktop sessions, so validators
// retain a monotonic cursor and consume only newer entries. Legacy entries do
// not carry IDs and therefore remain visible rather than being unsafely hidden.
func diagnosticsSince(report meadowBrowserDiagnosticsResponse, cursor int) meadowBrowserDiagnosticsResponse {
	if cursor <= 0 || len(report.Entries) == 0 {
		return report
	}
	filtered := make([]meadowBrowserDiagnostic, 0, len(report.Entries))
	for _, entry := range report.Entries {
		if entry.ID == 0 || entry.ID > cursor {
			filtered = append(filtered, entry)
		}
	}
	report.Entries = filtered
	report.ErrorCount = 0
	report.WarningCount = 0
	for _, entry := range filtered {
		switch strings.ToLower(strings.TrimSpace(entry.Level)) {
		case "error":
			report.ErrorCount++
		case "warn", "warning":
			report.WarningCount++
		}
	}
	return report
}

func browserRuntimeFailure(report meadowBrowserDiagnosticsResponse) string {
	errors := report.errors()
	if len(errors) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("BROWSER_RUNTIME_ERRORS: the rendered page emitted real browser failures.\n")
	for index, entry := range errors {
		if index >= 12 {
			fmt.Fprintf(&b, "- … %d additional error(s) omitted\n", len(errors)-index)
			break
		}
		kind := strings.TrimSpace(entry.Kind)
		if kind == "" {
			kind = "console"
		}
		fmt.Fprintf(&b, "- [%s] %s", kind, entry.Message)
		if entry.SourceURL != "" {
			fmt.Fprintf(&b, " @ %s", entry.SourceURL)
			if entry.Line > 0 {
				fmt.Fprintf(&b, ":%d", entry.Line)
				if entry.Column > 0 {
					fmt.Fprintf(&b, ":%d", entry.Column)
				}
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString("Repair the saved TypeScript/HTML source indicated by these diagnostics, rebuild the same artifact, then rerun the browser validator. Do not claim completion while any browser runtime error remains.")
	return strings.TrimSpace(b.String())
}

func browserRuntimeErrorResult(
	ws *Workspace,
	localHTML string,
	report meadowBrowserDiagnosticsResponse,
	contextLabel string,
) Result {
	message := browserRuntimeFailure(report)
	if contextLabel != "" {
		message = contextLabel + ":\n" + message
	}
	path := browserRepairModelPath(ws, localHTML, report.errors())
	facts := make([]map[string]any, 0)
	for index, entry := range report.errors() {
		if index == 12 {
			break
		}
		facts = append(facts, map[string]any{"message": entry.Message, "kind": entry.Kind,
			"source_url": entry.SourceURL, "line": entry.Line, "column": entry.Column})
	}
	result := errResult(&RecoveryError{
		Code:           "browser_runtime_errors",
		Message:        message,
		Path:           path,
		RequiredAction: "edit_reported_source_rebuild_and_rerun_browser_validator",
		PreserveDraft:  true,
		Diagnostics:    facts,
	})
	result.Display = "browser runtime errors detected"
	return result
}

func browserDiagnosticsUnavailableResult(err error) Result {
	result := errResult(&RecoveryError{
		Code:           "browser_diagnostics_unavailable",
		Message:        err.Error(),
		RequiredAction: "update_or_restart_meadow_browser_then_rerun_validator",
		PreserveDraft:  true,
	})
	result.Display = "browser diagnostics unavailable"
	return result
}

func browserRepairModelPath(
	ws *Workspace,
	localHTML string,
	errors []meadowBrowserDiagnostic,
) string {
	if strings.TrimSpace(localHTML) == "" {
		return ""
	}
	hostPath := localHTML
	preferTypeScript := true
	for _, entry := range errors {
		switch strings.TrimSpace(entry.Kind) {
		case "resource_error", "csp_violation", "navigation_error", "navigation_response_error":
			preferTypeScript = false
		}
	}
	if preferTypeScript && strings.EqualFold(filepath.Ext(localHTML), ".html") {
		candidate := strings.TrimSuffix(localHTML, filepath.Ext(localHTML)) + ".ts"
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			hostPath = candidate
		}
	}
	if ws != nil {
		return ws.ModelPath(hostPath)
	}
	return hostPath
}
