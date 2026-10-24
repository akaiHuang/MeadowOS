package tools

import "strings"

func (*MeadowBrowserWebMCP) ToolExecutionEffect(args map[string]any) string {
	// This fixed branch only extracts DOM from a retained page; invalid pins or
	// handles fail before extraction. Neither app declarations nor a purported
	// read-only callback may downgrade execute/discover to a read effect.
	if strings.EqualFold(strings.TrimSpace(strArg(args, "action")), "observe") &&
		strings.TrimSpace(strArg(args, "file_path")) == "" && strings.TrimSpace(strArg(args, "url")) == "" {
		return "read"
	}
	return ""
}
