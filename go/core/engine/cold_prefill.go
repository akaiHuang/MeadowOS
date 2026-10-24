package engine

import (
	"os"
	"strconv"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/mempalace"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/rag"
)

const prefillProfileColdSelectiveV1 = "cold-selective-v1"
const prefillProfileColdSelectiveV2 = "cold-selective-v2"

const coldToolResultBudgetRunes = 1600

func (e *Engine) activePrefillProfile() string {
	profile := strings.TrimSpace(e.PrefillProfile)
	if profile == "" {
		profile = strings.TrimSpace(os.Getenv("MEADOW_PREFILL_PROFILE"))
	}
	return strings.ToLower(profile)
}

func (e *Engine) combineMemoryForPrefill(tmpl *rag.Template, lessons []string) string {
	switch e.activePrefillProfile() {
	case prefillProfileColdSelectiveV1, prefillProfileColdSelectiveV2, "cold-prune-v1", "cold-compress-v1", "cold-prune-v2":
		return compactMemoryForColdPrefill(tmpl, lessons)
	default:
		return combineMemory(tmpl, lessons)
	}
}

func (e *Engine) shapeActionMessagesForPrefill(msgs []provider.Message) []provider.Message {
	profile := e.activePrefillProfile()
	if prefillProfileDisabled(profile) {
		return msgs
	}
	if provider.IsMeadowLocalModel(e.ModelID) || profile == prefillProfileColdSelectiveV2 || profile == "cold-prune-v2" {
		return compactToolResultsForColdPrefill(msgs, coldToolResultBudgetRunes)
	}
	return msgs
}

func prefillProfileDisabled(profile string) bool {
	switch profile {
	case "off", "0", "false", "no", "none", "raw":
		return true
	default:
		return false
	}
}

func compactMemoryForColdPrefill(tmpl *rag.Template, lessons []string) string {
	var parts []string
	if tmpl != nil {
		if strings.TrimSpace(tmpl.Content) != "" {
			parts = append(parts, compactExampleTemplate(tmpl))
		} else {
			parts = append(parts, compactKnowledgeTemplate(tmpl))
		}
	}
	if lessonBlock := compactLessons(lessons); lessonBlock != "" {
		parts = append(parts, lessonBlock)
	}
	return strings.Join(nonEmpty(parts), "\n\n")
}

func compactExampleTemplate(t *rag.Template) string {
	lines := []string{
		"【Cold prefill compact RAG】",
		"已匹配結構參考：" + strings.TrimSpace(t.Title) + "。",
		"不要塞入完整範例原文；請直接用 write_file 產出完整、可執行、內部一致的單一 HTML 檔案。",
		"必須保留的結構：完整 HTML 文件、內嵌 CSS/JS、主要畫面元素、事件監聽、主迴圈/狀態更新、錯誤或完成狀態。",
		"元素 id/class 與 JavaScript 查找必須一致；不要只寫骨架，不要反問用戶，不要只在對話裡貼程式碼。",
	}
	if len(t.Keywords) > 0 {
		lines = append(lines, "觸發關鍵字："+strings.Join(capStrings(t.Keywords, 8), ", ")+"。")
	}
	return strings.Join(lines, "\n")
}

func compactKnowledgeTemplate(t *rag.Template) string {
	var lines []string
	lines = append(lines,
		"【Cold prefill compact knowledge】",
		"參考知識："+strings.TrimSpace(t.Title)+"。",
	)
	if exec := truncateRunes(t.Execution, 900); exec != "" {
		lines = append(lines, "執行方式："+exec)
	}
	if len(t.Rubric) > 0 {
		var rubric []string
		for _, item := range capStrings(t.Rubric, 5) {
			if s := truncateRunes(item, 160); s != "" {
				rubric = append(rubric, "- "+s)
			}
		}
		if len(rubric) > 0 {
			lines = append(lines, "品質標準：\n"+strings.Join(rubric, "\n"))
		}
	}
	lines = append(lines, "直接完成任務；用合理預設值，不要反問用戶，不要只寫骨架。")
	return strings.Join(nonEmpty(lines), "\n")
}

func compactLessons(lessons []string) string {
	if len(lessons) == 0 {
		return ""
	}
	var lines []string
	for _, lesson := range capStrings(lessons, 3) {
		if s := truncateRunes(lesson, 160); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return mempalace.FormatForSystemPrompt(lines, nil)
}

func compactToolResultsForColdPrefill(msgs []provider.Message, budget int) []provider.Message {
	if budget <= 0 {
		return msgs
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	toolNames := make(map[string]string)
	lastTool := -1
	lastRead := -1
	for i, msg := range msgs {
		for _, call := range msg.ToolCalls {
			toolNames[call.ID] = call.Function.Name
		}
		if msg.Role != "tool" {
			continue
		}
		lastTool = i
		if toolNames[msg.ToolCallID] == "read_file" {
			lastRead = i
		}
	}
	for i := range out {
		if out[i].Role != "tool" {
			continue
		}
		// Keep the current observation and the most recent exact file read. A
		// small coding model may use that text as the source for an edit; replacing
		// it with a head/tail summary can turn the summary marker into file content.
		if i == lastTool || i == lastRead {
			continue
		}
		out[i].Content = compactToolResultText(out[i].Content, budget)
	}
	return out
}

func compactToolResultText(content string, budget int) string {
	text := strings.TrimSpace(content)
	r := []rune(text)
	if len(r) <= budget {
		return content
	}
	headBudget := budget * 3 / 5
	tailBudget := budget - headBudget
	if headBudget < 200 {
		headBudget = budget / 2
		tailBudget = budget - headBudget
	}
	head := strings.TrimSpace(string(r[:headBudget]))
	tail := strings.TrimSpace(string(r[len(r)-tailBudget:]))
	return strings.Join([]string{
		"[Meadow cold-prefill compacted tool result]",
		"This is context only. Never copy it into write_file/edit; re-read the exact range first.",
		"Original chars: " + strconv.Itoa(len(r)) + ".",
		"Head:",
		head,
		"Tail:",
		tail,
	}, "\n")
}

func capStrings(values []string, n int) []string {
	if n < 0 || len(values) <= n {
		return values
	}
	return values[:n]
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func nonEmpty(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}
