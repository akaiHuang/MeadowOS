package tools

import (
	"context"
	"net/http"

	"github.com/hey-meadow/meadow-code/go/core/desktopbrowserrpc"
)

const desktopBrowserPrivateBase = desktopbrowserrpc.BaseURL

// A complete tool execution retains its own document receipt. A concurrent
// execution may navigate this single view, but cannot make the first execution
// silently adopt its new document before evidence or an input is dispatched.
func desktopBrowserPageContext(ctx context.Context, base string) context.Context {
	if base == desktopBrowserPrivateBase {
		return desktopbrowserrpc.WithPageContext(ctx)
	}
	return ctx
}

// One embedded view belongs to the parent conversation. Child worktree tools
// retain their advertised schema and approval classification, while refusing
// to borrow the parent's page or start an unrelated Browser as a fallback.
type parentOnlyDesktopBrowser struct{ original Tool }

func (t *parentOnlyDesktopBrowser) Name() string                     { return t.original.Name() }
func (t *parentOnlyDesktopBrowser) Schema() map[string]any           { return t.original.Schema() }
func (t *parentOnlyDesktopBrowser) Kind() Kind                       { return t.original.Kind() }
func (t *parentOnlyDesktopBrowser) ParallelSafe(map[string]any) bool { return false }
func (t *parentOnlyDesktopBrowser) Description() string {
	return t.original.Description() + " The Desktop embedded Browser belongs to the parent conversation and is unavailable to concurrent worktree helpers. Return findings to the parent for Browser verification."
}
func (t *parentOnlyDesktopBrowser) Execute(context.Context, map[string]any) Result {
	return errResult(&RecoveryError{
		Code: "embedded_browser_parent_only", Message: "右側瀏覽器屬於主對話；並行子任務無法操作或讀取此頁面。",
		RequiredAction: "return_browser_verification_to_parent", PreserveDraft: true,
	})
}

// ConfigureDesktopBrowserTransport binds existing tool instances to a trusted
// parent capability. Mutating an existing WebMCP instance preserves its retained
// document/handle state across turns. No model argument chooses this transport.
// No tool is exposed here if the caller's registry intentionally excluded it.
func ConfigureDesktopBrowserTransport(registry *Registry, client *http.Client) {
	if registry == nil || client == nil {
		return
	}
	var checker *MeadowBrowserInteractionCheck
	if existing, ok := registry.Get("meadow_browser_interaction_check"); ok {
		checker, _ = existing.(*MeadowBrowserInteractionCheck)
	}
	for _, name := range []string{"meadow_browser_dom", "meadow_browser_visual_check", "meadow_browser_interaction_check", "meadow_browser_webmcp", "meadow_browser_suite", "meadow_browser_inspect"} {
		tool, exists := registry.Get(name)
		if !exists {
			continue
		}
		switch browser := tool.(type) {
		case *MeadowBrowserDOM:
			browser.BaseURL, browser.HTTP = desktopbrowserrpc.BaseURL, client
			browser.StartLocal, browser.RuntimeIdentity = nil, nil
		case *MeadowBrowserVisualCheck:
			browser.BaseURL, browser.HTTP = desktopbrowserrpc.BaseURL, client
			browser.StartLocal, browser.RestartLocal, browser.RuntimeIdentity = nil, nil, nil
		case *MeadowBrowserInteractionCheck:
			configureDesktopInteraction(browser, client)
		case *MeadowBrowserWebMCP:
			browser.BaseURL, browser.HTTP = desktopbrowserrpc.BaseURL, client
			browser.StartLocal, browser.RuntimeIdentity = nil, nil
		case *MeadowBrowserSuite:
			if checker != nil {
				browser.Checker = checker
			}
			configureDesktopInteraction(browser.Checker, client)
		case *MeadowBrowserInspect:
			if checker != nil {
				browser.Checker = checker
			}
			configureDesktopInteraction(browser.Checker, client)
		}
	}
}

func inheritDesktopBrowserTransport(result, base *Registry) {
	if base == nil {
		return
	}
	for _, name := range []string{"meadow_browser_webmcp", "meadow_browser_visual_check", "meadow_browser_dom", "meadow_browser_interaction_check"} {
		existing, ok := base.Get(name)
		if !ok {
			continue
		}
		var endpoint string
		var client *http.Client
		switch browser := existing.(type) {
		case *MeadowBrowserWebMCP:
			endpoint, client = browser.BaseURL, browser.HTTP
		case *MeadowBrowserVisualCheck:
			endpoint, client = browser.BaseURL, browser.HTTP
		case *MeadowBrowserDOM:
			endpoint, client = browser.BaseURL, browser.HTTP
		case *MeadowBrowserInteractionCheck:
			endpoint, client = browser.BaseURL, browser.HTTP
		}
		if endpoint == desktopBrowserPrivateBase && client != nil {
			ConfigureDesktopBrowserTransport(result, client)
			return
		}
	}
}

func configureDesktopInteraction(browser *MeadowBrowserInteractionCheck, client *http.Client) {
	if browser == nil {
		return
	}
	browser.BaseURL, browser.HTTP = desktopbrowserrpc.BaseURL, client
	browser.StartLocal, browser.NewWorker, browser.RuntimeIdentity = nil, nil, nil
}
