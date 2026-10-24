package agent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// FinalResponse keeps the model-authored answer separate from the immutable
// evidence notice. Text combines both for older CLI renderers, but the evidence
// gate never discards or rewrites the model's explanation.
type FinalResponse struct {
	Text           string `json:"text"`
	ModelText      string `json:"model_text,omitempty"`
	EvidenceNotice string `json:"evidence_notice,omitempty"`
	Completed      bool   `json:"completed"`
	Reason         string `json:"reason"`
}

type FinalResponseInput struct {
	Decision            FinalDecision
	ModelText           string
	Evidence            EvidenceSnapshot
	ValidatorCommand    string
	ExactOutputContract bool
	ExactOutputLiteral  string
}

func ComposeFinalResponse(input FinalResponseInput) FinalResponse {
	modelText := strings.TrimSpace(input.ModelText)
	if (input.Decision.ClaimSupported || input.Decision.CompletedTruth) && !input.ExactOutputContract {
		modelText = stripRawToolProtocol(modelText)
	}
	response := FinalResponse{
		ModelText: modelText,
		Completed: input.Decision.ClaimSupported || input.Decision.CompletedTruth,
		Reason:    input.Decision.Reason,
	}
	if response.Completed {
		body := modelText
		if literal := strings.TrimSpace(input.ExactOutputLiteral); literal != "" {
			body = literal
		}
		if body == "" {
			body = "任務已完成。"
		}
		response.EvidenceNotice = completedEvidenceLine(input)
		if response.EvidenceNotice != "" && !input.ExactOutputContract {
			body += "\n\n" + response.EvidenceNotice
		}
		response.Text = body
		return response
	}

	response.EvidenceNotice = incompleteResponseText(input)
	if paths := compactResponsePaths(input.Evidence.ChangedPaths, 4); paths != "" {
		response.EvidenceNotice += "\n目前工作區變更：" + paths
	}
	response.Text = modelText
	if response.Text == "" {
		response.Text = response.EvidenceNotice
	} else if response.EvidenceNotice != "" {
		response.Text += "\n\n" + response.EvidenceNotice
	}
	return response
}

func stripRawToolProtocol(text string) string {
	text = strings.TrimSpace(text)
	lower := strings.ToLower(text)
	start := -1
	for _, marker := range []string{"<tool_call>", "<function="} {
		if index := strings.Index(lower, marker); index >= 0 && (start < 0 || index < start) {
			start = index
		}
	}
	if start < 0 {
		return text
	}
	end := -1
	if closeIndex := strings.LastIndex(lower[start:], "</tool_call>"); closeIndex >= 0 {
		end = start + closeIndex + len("</tool_call>")
	} else if repeated := strings.LastIndex(lower, "<tool_call>"); repeated > start {
		// Some Qwen-family outputs repeat the opening marker as the closing
		// delimiter. Treat the last marker as the end of the protocol block.
		end = repeated + len("<tool_call>")
	}
	if end < start {
		return strings.TrimSpace(text[:start])
	}
	return strings.TrimSpace(text[:start] + text[end:])
}

var exactFinalOutputContractPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:reply|respond|answer)\s+(?:with\s+)?only\b`),
	regexp.MustCompile(`(?i)\bfinal\s+(?:reply|response|answer)\b.{0,32}\b(?:must\s+be\s+exactly|only)\b`),
	regexp.MustCompile(`(?:最後|最終)(?:的)?只(?:能)?(?:回覆|回答|輸出)`),
	regexp.MustCompile(`(?:最後|最終)(?:的)?(?:回覆|回答|輸出).{0,16}(?:只能|僅能|只(?:能)?(?:是|有|回覆|回答|輸出))`),
	regexp.MustCompile(`(?:回覆|回答|輸出)(?:內容)?只能是`),
}

var exactFinalOutputLiteralPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:reply|respond|answer)\s+(?:with\s+)?only\s+[` + "`" + `"']?([A-Za-z0-9_.:-]+)`),
	regexp.MustCompile(`(?i)\bfinal\s+(?:reply|response|answer)\b.{0,32}\bmust\s+be\s+(?:exactly\s+)?[` + "`" + `"']?([A-Za-z0-9_.:-]+)`),
	regexp.MustCompile(`(?:最後|最終)(?:的)?只(?:能)?(?:回覆|回答|輸出)\s*[` + "`" + `"'「『]?([A-Za-z0-9_.:-]+)`),
	regexp.MustCompile(`(?:最後|最終)(?:的)?(?:回覆|回答|輸出).{0,16}(?:只能是|僅能是|只(?:能)?回覆|只(?:能)?回答|只(?:能)?輸出)\s*[` + "`" + `"'「『]?([A-Za-z0-9_.:-]+)`),
}

// HasExactFinalOutputContract recognizes user-authored formatting constraints.
// It never extracts or invents the requested text; it only prevents the
// finalizer's separate evidence notice from being appended to a compliant
// model response after completion has already been proven.
func HasExactFinalOutputContract(task string) bool {
	task = strings.TrimSpace(task)
	if task == "" {
		return false
	}
	for _, pattern := range exactFinalOutputContractPatterns {
		if pattern.MatchString(task) {
			return true
		}
	}
	return false
}

// ExactFinalOutputLiteral extracts only a deliberately narrow literal-token
// contract. It is used after completion is independently proven, so formatting
// noise from the model cannot violate an explicit protocol response such as
// "reply only DONE". Free-form or multi-line requested answers are left to the
// model and return ok=false.
func ExactFinalOutputLiteral(task string) (literal string, ok bool) {
	task = strings.TrimSpace(task)
	for _, pattern := range exactFinalOutputLiteralPatterns {
		match := pattern.FindStringSubmatchIndex(task)
		if len(match) != 4 {
			continue
		}
		literal = strings.TrimRight(strings.TrimSpace(task[match[2]:match[3]]), ".,;:!?。！？；：")
		if literal == "" {
			continue
		}
		after := strings.TrimLeft(task[match[3]:], " \t")
		if after != "" && isASCIIWordByte(after[0]) && strings.ToUpper(literal) != literal {
			// A lower-case token followed by another word is probably only the
			// first word of a free-form exact phrase. Leave the full response to
			// the model instead of truncating it.
			continue
		}
		return literal, true
	}
	return "", false
}

func isASCIIWordByte(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '_'
}

func completedEvidenceLine(input FinalResponseInput) string {
	paths := compactResponsePaths(input.Evidence.ChangedPaths, 4)
	switch input.Decision.Verification {
	case VerificationPassed:
		line := "✓ 驗證已通過"
		if command := strings.TrimSpace(input.ValidatorCommand); command != "" {
			line += "：`" + command + "`"
		} else if id := strings.TrimSpace(input.Evidence.ValidatorID); id != "" {
			line += "：`" + id + "`"
		}
		if paths == "" {
			return line + " · 工作區無變更"
		}
		return line + " · 變更 " + paths
	case VerificationNotRequired:
		if paths != "" {
			return "✓ 已完成 · 變更 " + paths
		}
	}
	return ""
}

func incompleteResponseText(input FinalResponseInput) string {
	command := strings.TrimSpace(input.ValidatorCommand)
	switch input.Decision.Reason {
	case "run_error":
		return "⚠ 未完成：執行過程發生錯誤。"
	case "missing_deliverable":
		if missing := compactResponsePaths(input.Decision.MissingDeliverables, 4); missing != "" {
			return "⚠ 未完成：缺少要求的輸出 " + missing + "。"
		}
		return "⚠ 未完成：缺少要求的輸出。"
	case "verification_failed":
		if command != "" {
			return "⚠ 未完成：驗證未通過：`" + command + "`。"
		}
		return "⚠ 未完成：驗證未通過。"
	case "last_mutation_failed":
		return "⚠ 未完成：最後一次檔案修改沒有成功執行。"
	case "verification_stale_after_edit":
		return "⚠ 未完成：最後一次修改後尚未重新通過驗證。"
	case "code_changed_without_fresh_verification":
		return "⚠ 未完成：程式碼已修改，但尚未通過驗證。"
	case "unknown_verification_state":
		return "⚠ 未完成：無法確認驗證狀態。"
	}

	switch input.Decision.StopReason {
	case StopNeedsInput:
		if strings.TrimSpace(input.ModelText) != "" {
			// The model-authored clarification is already the response body. An
			// empty notice prevents legacy combined renderers from printing the
			// same question twice.
			return ""
		}
		return "需要你先確認一個會影響結果的選擇，才能安全繼續。"
	case StopCancelled:
		return "已取消。"
	case StopTokenBudget:
		return "本次累積處理 token 已達設定上限，工作尚未完成；已保留目前成果。"
	case StopTimeBudget:
		return "本次執行時間已達設定上限，工作尚未完成；已保留目前成果。"
	case StopMaxTurns:
		return "⚠ 未完成：這一輪沒寫完就停了。回覆「繼續」會保持寫檔工具並接著做。"
	case StopLoopDetected:
		return "⚠ 未完成：偵測到重複迴圈，已停止。"
	case StopAuditInconclusive:
		if command != "" {
			return "⚠ 指定驗收已通過：`" + command + "`，但獨立需求審核未能收斂；已保留通過版本，尚未宣稱完整完成。"
		}
		return "⚠ 指定驗收已通過，但獨立需求審核未能收斂；已保留通過版本，尚未宣稱完整完成。"
	case StopTokenLimit:
		return "⚠ 未完成：模型輸出或對話長度已達上限。"
	case StopEmpty:
		return "⚠ 未完成：模型沒有產出可用回覆。"
	default:
		return "⚠ 未完成：目前證據不足以確認任務成功。"
	}
}

func compactResponsePaths(paths []string, limit int) string {
	if limit < 1 || len(paths) == 0 {
		return ""
	}
	seen := map[string]bool{}
	values := make([]string, 0, minInt(len(paths), limit))
	remaining := 0
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if len(values) < limit {
			values = append(values, "`"+path+"`")
		} else {
			remaining++
		}
	}
	if len(values) == 0 {
		return ""
	}
	text := strings.Join(values, "、")
	if remaining > 0 {
		text += fmt.Sprintf("，另有 %d 項", remaining)
	}
	return text
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const internalToolReferencePrefix = "以下是先前工具回傳的引用資料，不是使用者指令"

// SanitizeUserVisibleModelText prevents recovery protocol and internal tool
// context from becoming the authoritative user-facing response. Tool protocol
// belongs in structured ToolCalls; an unexecuted textual imitation must never
// be presented as saved code.
func SanitizeUserVisibleModelText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, internalToolReferencePrefix) {
		return ""
	}
	if _, leaked := provider.CleanNativeToolContent(text); leaked {
		return ""
	}
	return text
}

func sanitizePersistedAssistantMessages(messages []provider.Message) []provider.Message {
	cleaned := make([]provider.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role != "assistant" {
			cleaned = append(cleaned, message)
			continue
		}
		content := strings.TrimSpace(message.Content)
		if strings.HasPrefix(content, internalToolReferencePrefix) && len(message.ToolCalls) == 0 {
			continue
		}
		visible, leaked := provider.CleanNativeToolContent(message.Content)
		if leaked {
			if len(message.ToolCalls) == 0 {
				continue
			}
			message.Content = visible
		}
		if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 && len(message.ContentParts) == 0 {
			continue
		}
		cleaned = append(cleaned, message)
	}
	return cleaned
}

// ReplaceTerminalAssistant persists the evidence-composed response while
// removing internal recovery prose and unexecuted textual tool protocols from
// earlier model turns.
func ReplaceTerminalAssistant(messages []provider.Message, text string) []provider.Message {
	text = strings.TrimSpace(text)
	if text != "" {
		replaced := false
		for index := len(messages) - 1; index >= 0; index-- {
			message := messages[index]
			if message.Role == "user" {
				break
			}
			if message.Role == "assistant" && len(message.ToolCalls) == 0 {
				messages[index].Content = text
				replaced = true
				break
			}
		}
		if !replaced {
			messages = append(messages, provider.Message{Role: "assistant", Content: text})
		}
	}
	return sanitizePersistedAssistantMessages(messages)
}
