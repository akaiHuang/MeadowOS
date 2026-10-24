package tools

// ExecutionEffectProvider is a trusted implementation contract for fixed
// operations whose effect differs from their permission Kind. It never derives
// authority from app annotations or model prose. The executor recognizes only
// "read" for an execute-kind tool; every other value keeps the normal effect.
// Kind and all permission/subtask boundaries remain unchanged.
type ExecutionEffectProvider interface {
	ToolExecutionEffect(args map[string]any) string
}
