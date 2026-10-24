package agent

import "github.com/hey-meadow/meadow-code/go/core/provider"

func recoveryErrorToolName(e *provider.ToolCallRecoveryError) string {
	if e == nil {
		return ""
	}
	return e.ToolName
}
func recoveryErrorReason(e *provider.ToolCallRecoveryError) string {
	if e == nil {
		return "malformed_tool_call"
	}
	return string(e.Reason)
}
func recoveryErrorMissingFields(e *provider.ToolCallRecoveryError) []string {
	if e == nil {
		return nil
	}
	return e.MissingRequired
}
