package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	MemorySearchToolName   = "memory_search"
	MemoryRememberToolName = "memory_remember"
)

// MemorySearchTool keeps ordinary same-session recall automatic and cheap,
// while making the wider cross-session privacy boundary a visible user choice.
type MemorySearchTool struct {
	Store *CartridgeStore
	Scope CartridgeScope
	Ask   tools.Asker
}

func (t *MemorySearchTool) Name() string { return MemorySearchToolName }
func (t *MemorySearchTool) Description() string {
	return "Search explicit Meadow memories. Same-session recall is local; cross-session recall always asks the user for permission before searching."
}
func (t *MemorySearchTool) Kind() tools.Kind { return tools.KindReadOnly }
func (t *MemorySearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "The exact fact, preference, decision, or correction to recall."},
			"scope": map[string]any{"type": "string", "enum": []string{"current_session", "cross_session"}},
		},
		"required":             []string{"query", "scope"},
		"additionalProperties": false,
	}
}

func (t *MemorySearchTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	if t == nil || t.Store == nil {
		return documentToolError("memory store is unavailable")
	}
	query, _ := args["query"].(string)
	scope, _ := args["scope"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return documentToolError("query is required")
	}
	allowCross := scope == "cross_session"
	if allowCross {
		if t.Ask == nil {
			return documentToolError("cross-session memory search requires interactive user permission")
		}
		answers, err := t.Ask(ctx, []tools.Question{{
			Header:   "跨對話記憶",
			Question: fmt.Sprintf("要搜尋其他對話中與「%s」相關、由你明示保存的記憶嗎？", truncateRunes(query, 80)),
			Options: []tools.QuestionOption{
				{Label: "允許這次搜尋", Description: "只在本機搜尋已明示保存的跨對話記憶。"},
				{Label: "不要搜尋", Description: "只保留目前對話範圍。"},
			},
		}})
		if err != nil || len(answers) == 0 || strings.TrimSpace(answers[0]) != "允許這次搜尋" {
			return tools.Result{LLMContent: "CROSS_SESSION_MEMORY_DECLINED: Continue without cross-session memory.", Display: "未搜尋跨對話記憶"}
		}
	}
	origins := []CartridgeOrigin{CartridgeSessionMemory}
	if allowCross {
		origins = []CartridgeOrigin{CartridgeUserMemory}
	}
	matches := t.Store.Search(ctx, CartridgeQuery{
		Text: query, Scope: t.Scope, Origins: origins, AllowCrossSession: allowCross,
		Limit: 4, MaxTokens: 700,
	})
	if len(matches) == 0 {
		return tools.Result{LLMContent: "NO_SAVED_MEMORY_MATCH: No explicit saved memory matched this request.", Display: "沒有符合的已保存記憶"}
	}
	text := CompactCartridgeEvidence(matches, 1800)
	return tools.Result{LLMContent: text, Display: fmt.Sprintf("找到 %d 筆已保存記憶", len(matches))}
}

// MemoryRememberTool never infers importance from repetition or model prose.
// It stores the proposed text only after an explicit product-surface approval.
type MemoryRememberTool struct {
	Store *CartridgeStore
	Scope CartridgeScope
	Ask   tools.Asker
}

func (t *MemoryRememberTool) Name() string { return MemoryRememberToolName }
func (t *MemoryRememberTool) Description() string {
	return "Propose an exact user fact, preference, decision, or correction for local memory. Nothing is stored until the user confirms the exact text and scope."
}
func (t *MemoryRememberTool) Kind() tools.Kind { return tools.KindInteract }
func (t *MemoryRememberTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":      map[string]any{"type": "string", "description": "Short factual label."},
			"memory":     map[string]any{"type": "string", "description": "Exact fact or preference to preserve; do not include model inference."},
			"scope":      map[string]any{"type": "string", "enum": []string{"current_session", "cross_session"}},
			"supersedes": map[string]any{"type": "string", "description": "Optional prior memory record id explicitly corrected by this text."},
		},
		"required":             []string{"title", "memory", "scope"},
		"additionalProperties": false,
	}
}

func (t *MemoryRememberTool) Execute(ctx context.Context, args map[string]any) tools.Result {
	if t == nil || t.Store == nil {
		return documentToolError("memory store is unavailable")
	}
	if t.Ask == nil {
		return documentToolError("saving memory requires interactive user confirmation")
	}
	title, _ := args["title"].(string)
	memory, _ := args["memory"].(string)
	scopeName, _ := args["scope"].(string)
	supersedes, _ := args["supersedes"].(string)
	title, memory = strings.TrimSpace(title), strings.TrimSpace(memory)
	if title == "" || memory == "" {
		return documentToolError("title and memory are required")
	}
	if len(memory) > 4000 {
		return documentToolError("memory is too large; preserve one focused fact or decision")
	}
	description := "只在目前對話使用。"
	if scopeName == "cross_session" {
		description = "未來可在你明示允許跨對話搜尋時使用。"
	}
	answers, err := t.Ask(ctx, []tools.Question{{
		Header:   "保存記憶",
		Question: fmt.Sprintf("要記住以下內容嗎？\n\n%s\n\n%s", memory, description),
		Options: []tools.QuestionOption{
			{Label: "記住這項內容", Description: description},
			{Label: "不要記住", Description: "不寫入任何記憶。"},
		},
	}})
	if err != nil || len(answers) == 0 || strings.TrimSpace(answers[0]) != "記住這項內容" {
		return tools.Result{LLMContent: "MEMORY_SAVE_DECLINED: Nothing was stored.", Display: "未保存記憶"}
	}
	origin := CartridgeSessionMemory
	scope := t.Scope
	if scopeName == "cross_session" {
		origin = CartridgeUserMemory
		scope.SessionID = ""
	} else if scope.SessionID == "" {
		return documentToolError("current session id is unavailable")
	}
	digest := sha256.Sum256([]byte(string(origin) + "\x00" + title + "\x00" + memory + "\x00" + scope.SessionID))
	record := CartridgeRecord{
		ID: "memory-" + hex.EncodeToString(digest[:8]), CartridgeID: string(origin),
		Title: title, Origin: origin, EvidenceKind: EvidenceExplicitUser,
		Body: memory, Scope: scope, ReviewStatus: "reviewed", Enabled: true,
	}
	if value := strings.TrimSpace(supersedes); value != "" {
		record.Supersedes = []string{value}
	}
	stored, err := t.Store.Upsert(ctx, record)
	if err != nil {
		return documentToolError("save memory: %v", err)
	}
	return tools.Result{
		LLMContent: fmt.Sprintf("MEMORY_SAVED id=%s scope=%s. Do not claim any additional memory was stored.", stored.ID, scopeName),
		Display:    "已保存：" + title,
	}
}
