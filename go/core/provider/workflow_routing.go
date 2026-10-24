package provider

import (
	"context"
	"regexp"
	"strings"
)

// StreamClient is the provider surface needed by WorkflowRoutedClient.
// *Client satisfies it; the small interface keeps routing independently
// testable and avoids coupling provider to the engine package.
type StreamClient interface {
	ChatStream(context.Context, ChatRequest, *StreamHooks) (*StreamResult, error)
}

// WorkflowRoute identifies the adapter selected for one model turn.
type WorkflowRoute string

const (
	WorkflowRouteSWE         WorkflowRoute = "swe"
	WorkflowRouteAgentKernel WorkflowRoute = "agent_kernel"
)

// WorkflowRoutedClient keeps broad SWE behavior and syntax-sensitive tool
// emission in separate adapters.  The route depends only on observable action
// state, never prompt text, repository identity, or benchmark case ids.
type WorkflowRoutedClient struct {
	SWE         StreamClient
	AgentKernel StreamClient
	OnRoute     func(WorkflowRoute)
}

func NewWorkflowRoutedClient(swe, agentKernel StreamClient) *WorkflowRoutedClient {
	return &WorkflowRoutedClient{SWE: swe, AgentKernel: agentKernel}
}

func (c *WorkflowRoutedClient) ChatStream(ctx context.Context, req ChatRequest, hooks *StreamHooks) (*StreamResult, error) {
	route := WorkflowRouteSWE
	client := c.SWE
	if ShouldRouteAgentKernel(req.Messages) {
		route = WorkflowRouteAgentKernel
		client = c.AgentKernel
		// The kernel endpoint can expose a different adapter-qualified served
		// model id.  An empty request model lets its own Client fill that id.
		req.Model = ""
	}
	if c.OnRoute != nil {
		c.OnRoute(route)
	}
	return client.ChatStream(ctx, req, hooks)
}

var explicitValidationCommandRe = regexp.MustCompile("(?is)(?:run|execute|執行|执行|跑)\\s*(?:exactly\\s*)?`(?:python3?|pytest|cargo|ruby|npm|pnpm|yarn|node|bash|sh|go\\s+test)\\b[^`]*`")
var coreValidationCommandRe = regexp.MustCompile("(?is)Exact validation command:\\s*(?:python3?|pytest|cargo|ruby|npm|pnpm|yarn|node|bash|sh|go\\s+test)\\b")

// ShouldRouteAgentKernel recognizes the narrow "successful edit completed and
// the user supplied an exact validator command" state. The kernel was trained
// to reproduce a visible command precisely; it must not invent a validator for
// an open-ended SWE issue. Discovery, semantic command selection, rejected
// edits, failures, and user steering remain on the broad SWE adapter.
func ShouldRouteAgentKernel(messages []Message) bool {
	if len(messages) < 2 {
		return false
	}
	hasExactValidator := false
	for _, message := range messages {
		if message.RuntimeObservation {
			continue
		}
		if (message.Role == "user" && explicitValidationCommandRe.MatchString(message.Content)) ||
			(message.Role == "system" && coreValidationCommandRe.MatchString(message.Content)) {
			hasExactValidator = true
			break
		}
	}
	if !hasExactValidator {
		return false
	}
	resultIndex := len(messages) - 1
	for resultIndex >= 0 && messages[resultIndex].RuntimeObservation {
		resultIndex--
	}
	if resultIndex < 0 {
		return false
	}
	result := messages[resultIndex]
	if result.Role != "tool" || !toolResultSucceeded(result.Content) {
		return false
	}

	for index := resultIndex - 1; index >= 0; index-- {
		message := messages[index]
		if message.RuntimeObservation {
			continue
		}
		if message.Role == "user" {
			return false
		}
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		for _, call := range message.ToolCalls {
			if call.ID == result.ToolCallID {
				return call.Function.Name == "edit"
			}
		}
		return false
	}
	return false
}

func toolResultSucceeded(content string) bool {
	value := strings.ToLower(strings.TrimSpace(content))
	if value == "" {
		return false
	}
	for _, marker := range []string{
		"error:", "failed", "denied", "rejected", "not found", "no occurrence", "exit code ",
	} {
		if strings.Contains(value, marker) {
			return false
		}
	}
	return true
}
