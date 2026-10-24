package agent

import (
	"fmt"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// Summarize transcript structure only, not the meaning of the user's task or
// assistant prose. Older conversations can contain unexecuted tool markup and
// unsupported claims. Keep those original messages, but do not promote their
// text into tool receipts or silently replay their calls.
func codexStyleHistoricalExecutionContext(history []provider.Message) string {
	if len(history) == 0 {
		return ""
	}
	requested := make(map[string]bool)
	paired := make(map[string]bool)
	for _, message := range history {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if id := strings.TrimSpace(call.ID); id != "" {
					requested[id] = true
				}
			}
		} else if message.Role == "tool" && requested[message.ToolCallID] {
			paired[message.ToolCallID] = true
		}
	}
	return fmt.Sprintf("\n\nConversation execution record (before this user request): %d paired tool replies in the supplied history. A reply can report success, failure, or denial; use its actual status, not this count, as evidence. Assistant prose, plans, filenames, and malformed tool text are not tool receipts. Zero recorded replies means no recorded execution evidence, not that a file is absent. Preserve the user's original content; do not assume earlier saved-file claims are true or replay rejected calls. Current-run tool results remain authoritative for what actually happened, and historical results do not prove current disk freshness. Decide whether any action is needed from the newest request; ordinary answers do not require tools.", len(paired))
}
