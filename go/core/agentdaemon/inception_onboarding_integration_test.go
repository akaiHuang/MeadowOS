//go:build linux && meadow_vm_lab && !meadow_protected

package agentdaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/daemontransport"
	"github.com/hey-meadow/meadow-code/go/core/engine"
	"github.com/hey-meadow/meadow-code/go/core/modelconnection"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/runtimecontrol"
)

// This integration uses the real authenticated Unix listener, Agent host,
// native configuration lease, catalog/credential store, session admission,
// queue, and harness. The model is an in-process fixture: no provider HTTP
// request, real API key, desktop interaction, or claimed inference acceptance.
type inceptionOnboardingModel struct {
	requests chan provider.ChatRequest
	release  chan struct{}
}

func (m *inceptionOnboardingModel) ChatStream(ctx context.Context, req provider.ChatRequest, hooks *provider.StreamHooks) (*provider.StreamResult, error) {
	select {
	case m.requests <- req:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.release:
	}
	content := "Synthetic onboarding fixture completed."
	if hooks != nil && hooks.OnDelta != nil {
		hooks.OnDelta(content, false)
	}
	return &provider.StreamResult{Content: content, FinishReason: "stop", Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
}

func TestInceptionOnboardingRealUnixAgentPreservesOldChats(t *testing.T) {
	// Keep the Unix socket path below Linux's sockaddr_un size limit, even
	// when the test runner uses long test names or a deeply nested TMPDIR.
	root, err := os.MkdirTemp("/tmp", "mios-onboard-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	state, workspace := filepath.Join(root, "state"), filepath.Join(root, "work")
	for _, path := range []string{state, workspace} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	catalogPath := filepath.Join(state, "model-connections-v1.json")
	socketPath := filepath.Join(state, "run", "agent.sock")
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const key = "synthetic-onboarding-NOT-A-REAL-API-KEY"
	for name, value := range map[string]string{
		"HOME": root, "MEADOW_HOME": state, "MEADOW_MODEL_CONNECTIONS_FILE": catalogPath,
		"MEADOW_AGENT_SOCKET": socketPath, "MEADOW_AGENT_API_KEY": token,
		"MEADOW_AGENT_TOKEN_FILE": filepath.Join(state, "run", "agent-api-token"),
		"XDG_DATA_HOME":           filepath.Join(root, "data"), "XDG_STATE_HOME": filepath.Join(root, "state-xdg"),
		"MEADOW_EXPECTED_TEAM_ID": "", "MEADOW_PUBLIC_RELEASE": "", "MEADOW_HARNESS_POLICY_FILE": "",
		"MEADOW_ARTIFACT_RETRIEVAL": "off", "MEADOW_BROWSER_BASE": "", "MEADOW_BROWSER_URL": "", "MEADOW_BROWSER_BIN": "",
	} {
		t.Setenv(name, value)
	}
	adapter, err := runtimecontrol.NewExternalAdapter(catalogPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &inceptionOnboardingModel{requests: make(chan provider.ChatRequest, 8), release: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(fixture.release)
		}
	})
	handler, err := NewHandler(Config{WorkspaceRoot: workspace, StateRoot: state, ModelID: "unconfigured",
		ConversationProfile: "meadow-os", HarnessMode: "codex-style", ApprovalMode: engine.ModeFullAccess}, Dependencies{
		RuntimeControl: adapter,
		ModelSource:    ModelSourceFunc(func(string) (agentcore.KernelModel, error) { return fixture, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.(interface{ Close() error }).Close() })
	protected, err := daemontransport.ProtectedHandlerWithConfig(handler, daemontransport.HealthConfig{
		StateRoot: state, ModelID: "unconfigured", ConnectionMode: true,
		AppliedConnectionIdentity: func() (string, string, string) {
			identity := adapter.ConfigurationIdentity()
			return identity.ConnectionID, identity.ModelID, identity.Fingerprint
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := daemontransport.Listen()
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: protected}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	call := func(method, path string, body any, authorized bool) (int, map[string]any) {
		t.Helper()
		var input io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, "http://meadow-agent.local"+path, input)
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(key)) {
			t.Fatal("Agent response disclosed the synthetic provider credential")
		}
		var result map[string]any
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("invalid JSON for %s %s (HTTP %d)", method, path, response.StatusCode)
		}
		return response.StatusCode, result
	}
	if code, _ := call(http.MethodPost, "/agent/configuration/lease", nil, false); code != http.StatusUnauthorized {
		t.Fatal("configuration lease was available without the independent Agent credential")
	}
	code, old := call(http.MethodPost, "/session", map[string]any{"cwd": workspace, "sessionScope": "thread", "approvalChannel": "interactive"}, true)
	if code != http.StatusOK || old["modelId"] != "unconfigured" || old["connectionId"] != "" {
		t.Fatal("initial unconfigured session was not created")
	}
	oldID, ok := old["sessionId"].(string)
	if !ok || oldID == "" {
		t.Fatal("initial session identity missing")
	}
	acquire := func() string {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			code, result := call(http.MethodPost, "/agent/configuration/lease", nil, true)
			if code == http.StatusOK {
				lease, ok := result["leaseId"].(string)
				if !ok || len(lease) != 64 {
					t.Fatal("invalid native lease receipt")
				}
				return lease
			}
			if code != http.StatusConflict || time.Now().After(deadline) {
				t.Fatalf("native Agent did not become idle (HTTP %d)", code)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	configure := func(apiKey string) string {
		t.Helper()
		lease := acquire()
		code, result := call(http.MethodPost, "/agent/connections/inception", map[string]string{"leaseId": lease, "apiKey": apiKey}, true)
		if code != http.StatusOK || result["applied"] != true || result["modelId"] != "mercury-3" || result["supportsTools"] != true {
			t.Fatal("native Inception transaction did not apply complete capabilities")
		}
		if code, _ := call(http.MethodDelete, "/agent/configuration/lease", map[string]string{"leaseId": lease}, true); code != http.StatusOK {
			t.Fatal("native configuration lease was not released")
		}
		identity, ok := result["connectionId"].(string)
		if !ok || identity == "" {
			t.Fatal("configured connection identity missing")
		}
		return identity
	}
	firstConnection := configure(key)
	if code, identity := call(http.MethodGet, "/transport/identity", nil, true); code != http.StatusOK || identity["connection_id"] != firstConnection || identity["model_id"] != "mercury-3" {
		t.Fatal("transport identity did not observe native configuration publication")
	}
	if code, context := call(http.MethodGet, "/session/"+oldID+"/context", nil, true); code != http.StatusOK || context["connectionId"] != "" || context["running"] != false {
		t.Fatal("old live empty session lost its immutable connection binding")
	}
	prompt := map[string]any{"prompt": []map[string]string{{"type": "text", "text": "Say hello for this synthetic onboarding check."}}, "options": map[string]string{"mode": "auto"}}
	if code, _ := call(http.MethodPost, "/session/"+oldID+"/queue", prompt, true); code != http.StatusConflict {
		t.Fatal("unconfigured old session was silently rebound")
	}
	code, fresh := call(http.MethodPost, "/session", map[string]any{"cwd": workspace, "sessionScope": "thread", "approvalChannel": "interactive"}, true)
	if code != http.StatusOK || fresh["modelId"] != "mercury-3" || fresh["connectionId"] != firstConnection {
		t.Fatal("new session did not bind to the configured Mercury 3 route")
	}
	freshID := fresh["sessionId"].(string)
	if code, _ := call(http.MethodPost, "/session/"+freshID+"/queue", prompt, true); code < 200 || code >= 300 {
		t.Fatal("configured session did not accept a prompt")
	}
	select {
	case req := <-fixture.requests:
		if req.Model != "mercury-3" || len(req.Tools) == 0 {
			t.Fatal("actual native harness request omitted Mercury 3 or tool definitions")
		}
		serialized, _ := json.Marshal(req)
		if strings.Contains(string(serialized), key) {
			t.Fatal("provider credential entered a model prompt")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("durable queue did not reach the in-process model fixture")
	}
	if code, _ := call(http.MethodPost, "/agent/configuration/lease", nil, true); code != http.StatusConflict {
		t.Fatal("native lease allowed reconfiguration during an active model turn")
	}
	close(fixture.release)
	released = true
	secondConnection := configure(key + "-rotated")
	if secondConnection == firstConnection {
		t.Fatal("credential rotation reused an existing chat identity")
	}
	if code, context := call(http.MethodGet, "/session/"+freshID+"/context", nil, true); code != http.StatusOK || context["connectionId"] != firstConnection {
		t.Fatal("submitted chat was rebound or discarded during credential rotation")
	}
	if code, _ := call(http.MethodPost, "/session/"+freshID+"/queue", prompt, true); code != http.StatusConflict {
		t.Fatal("submitted chat accepted a different credential identity")
	}
	code, history := call(http.MethodGet, "/sessions?cwd="+url.QueryEscape(workspace), nil, true)
	if code != http.StatusOK {
		t.Fatal("stored history unavailable after rotation")
	}
	found := false
	for _, row := range history["sessions"].([]any) {
		if row.(map[string]any)["sessionId"] == freshID {
			found = true
		}
	}
	if !found {
		t.Fatal("submitted old chat disappeared from stored history")
	}
	if saved, err := modelconnection.Credential(catalogPath, firstConnection); err != nil || saved != key {
		t.Fatal("rotation replaced the submitted chat's immutable saved credential")
	}
}
