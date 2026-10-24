package harness

import (
	"context"
	"github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/sessionrecovery"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

// Binding contains the durable and live identities required to bind one UI
// transport to a Harness Core runtime. It contains no CLI, Desktop or SSE
// concepts.
type Binding struct {
	SessionID string
	ChatID    string
	CWD       string
	ModelID   string
	// ConnectionID scopes a served model name to its selected endpoint.
	// Empty retains the legacy single-Core deployment binding.
	ConnectionID string
	// HarnessMode is the immutable, resolved mode admitted for this run.
	// Empty preserves the host default for legacy callers.
	HarnessMode         string
	ContextWindowTokens int
	PromptTokenLimit    int
	SystemPrompt        string
	ContextManager      *agent.ContextManager
	Registry            *tools.Registry
	DisabledTools       []string
	AskUser             tools.Asker
	Authorize           agent.AuthorizeFunc
	RequireToolAccount  func(context.Context) error
}

// Events are transport-neutral facts emitted by Harness Core. UI adapters may
// render them as terminal messages, SSE records or native Desktop components.
type Events struct {
	OnMessageBoundary   func(string) string
	OnEvo               func(map[string]any)
	OnContent           func(string)
	OnThought           func(string)
	OnProgress          func(provider.InferenceProgress)
	OnToolDraft         func(string, int)
	OnToolDraftProgress func(provider.ToolDraftProgress)
	OnToolCall          func(string, map[string]any)
	OnToolResult        func(string, tools.Result)
	OnToolStart         func(agent.ToolStart)
	OnToolComplete      func(agent.ToolExecution)
	OnTurn              func(int, *provider.Usage)
	OnGuidance          func(agent.GuidanceEvent)
	OnCheckpoint        func([]provider.Message)
	OnResumePlan        func(sessionrecovery.ResumePlan)
}

// RuntimeFactory may return (nil, nil) when a model family intentionally uses
// a different runtime implementation.
type RuntimeFactory func(Binding, Events) (agent.Runtime, error)
