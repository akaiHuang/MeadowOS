package agentdaemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

type inceptionTestLease struct {
	accepted bool
	calls    int
}

func (l *inceptionTestLease) ApplyConfigurationLease(id string, apply func() (string, error)) error {
	l.calls++
	if !l.accepted || id != strings.Repeat("a", 64) {
		return errors.New("agent_not_idle")
	}
	_, err := apply()
	return err
}

func TestInceptionConfigurationLeaseAndSanitizedReceipt(t *testing.T) {
	for _, idle := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "connections.json")
		adapter, _ := runtimecontrol.NewExternalAdapter(path, "", "")
		lease := &inceptionTestLease{accepted: idle}
		key := "synthetic-API-key-NOT-A-REAL-CREDENTIAL"
		body, _ := json.Marshal(map[string]string{"leaseId": strings.Repeat("a", 64), "apiKey": key})
		r := httptest.NewRequest(http.MethodPost, "/agent/connections/inception", bytes.NewReader(body))
		w := httptest.NewRecorder()
		inceptionConfigurationHandler(adapter, lease).ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), key) {
			t.Fatal("API response leaked a credential")
		}
		if !idle {
			if w.Code != http.StatusConflict {
				t.Fatalf("busy setup status: %d", w.Code)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("busy setup modified state")
			}
			continue
		}
		if w.Code != http.StatusOK {
			t.Fatalf("setup status: %d", w.Code)
		}
		var receipt map[string]any
		if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt["applied"] != true || receipt["modelId"] != "mercury-3" || receipt["supportsTools"] != true || receipt["supportsImages"] != false || receipt["reasoningControl"] != "inception-effort" {
			t.Fatal("missing complete applied capabilities")
		}
	}
}

func TestInceptionConfigurationRejectsMalformedRequestsBeforeLease(t *testing.T) {
	for _, body := range []string{
		`{"leaseId":"short","apiKey":"synthetic-fixture"}`,
		`{"leaseId":"` + strings.Repeat("a", 64) + `","apiKey":"fake\nkey"}`,
		`{"leaseId":"` + strings.Repeat("a", 64) + `","apiKey":"synthetic-fixture","baseUrl":"https://untrusted.invalid"}`,
		`{"leaseId":"` + strings.Repeat("a", 64) + `","apiKey":"synthetic-fixture"} {}`,
		`{"leaseId":"` + strings.Repeat("a", 64) + `","apiKey":"` + strings.Repeat("x", 21<<10) + `"}`,
	} {
		path := filepath.Join(t.TempDir(), "connections.json")
		adapter, _ := runtimecontrol.NewExternalAdapter(path, "", "")
		lease := &inceptionTestLease{accepted: true}
		w := httptest.NewRecorder()
		inceptionConfigurationHandler(adapter, lease).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/agent/connections/inception", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest || lease.calls != 0 {
			t.Fatal("malformed request reached mutation boundary")
		}
		if strings.Contains(w.Body.String(), "synthetic-fixture") {
			t.Fatal("malformed request leaked a key")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("malformed request wrote state")
		}
	}
}
