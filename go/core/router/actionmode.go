// Action-turn machinery: the Go port of the local-Gemma-4 request rewriting
// in provider/mlx.ts. For a local 26B, an action turn does not get the big
// system prompt — it gets a purpose-built mini prompt, a pruned toolset, a
// forced tool_choice, and (once the plan's tools have all run) a final
// summary turn with tools removed.
package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// --- turn-type predicates (ports of the shouldXxx helpers in mlx.ts) ---

var (
	keepToolsRe = regexp.MustCompile(`(?i)\b(read|write|edit|modify|fix|implement|create|delete|run|execute|test|build|commit|git|npm|bun|python|node|file|repo|diff|patch|shell|terminal|web|internet|online|latest|recent|weather|news|price|pricing|stock|exchange rate|benchmark|score|ranking|schedule|release|version|law|regulation|policy|crawl|scrape|website|webpage|20(2[5-9]|[3-9][0-9]))\b|https?://|幫我|修|改|寫|建立|執行|測試|檔案|專案|做一個|做個|查資料|找資料|研究.*網站|爬蟲|爬取|網站|網頁|最新|最近|今天|今日|現任|新聞|價格|價錢|股價|股票|匯率|天氣|天气|跑分|分數|評測|評價|排名|賽程|發布|釋出|上市|法規|法律|政策|存下來|存下来|存成|存到|存為|存为|儲存|储存|保存|commit`)

	contextualFollowUpRe = regexp.MustCompile(`(?i)我沒看到|沒看到|沒有看到|沒看見|沒有看見|在哪|哪裡|哪里|剛剛|刚刚|上一|前面|你剛|你刚|你沒有|你没|沒做|没做|沒有做|存下來|存下来|儲存|储存|保存|忘記上文|忘记上文|上文|重來|重来|目前進度|進度\??|做到哪|寫了沒|完成了嗎|\b(i do not see|i don't see|not seeing|where is it|where did|you did not|you didn't|save it|save this|previous|last|above|again|context|progress)\b`)

	progressOrContinueRe = regexp.MustCompile(`(?is)^(?:目前)?進度[？?。.!！]*$|^(?:做到哪(?:裡|里)?|寫了沒|完成了嗎|繼續(?:做|寫|吧)?|接著(?:做|寫)|continue)[？?。.!！]*$`)

	requireToolRe        = regexp.MustCompile(`(?i)\b(write|edit|modify|fix|implement|create|delete|run|execute|test|build|commit|git|npm|bun|python|node|file|repo|diff|patch|shell|terminal|mkdir|touch|not seeing|where is it|you did not|you didn't|save it|save this|web|internet|online|latest|recent|weather|news|price|pricing|stock|exchange rate|benchmark|score|ranking|schedule|release|version|law|regulation|policy|crawl|scrape|website|webpage|20(2[5-9]|[3-9][0-9]))\b|https?://|開一個|开一个|建立|創建|创建|新增|寫入|写入|寫.*檔|写.*档|檔案|文件|修|改|執行|执行|測試|测试|跑|做一個|做个|做個|幫我做|帮我做|查資料|找資料|研究.*網站|爬蟲|爬取|網站|網頁|最新|最近|今天|今日|現任|新聞|價格|價錢|股價|股票|匯率|天氣|天气|跑分|分數|評測|評價|排名|賽程|發布|釋出|上市|法規|法律|政策|存下來|存下来|存成|存到|存為|存为|儲存|储存|保存|我沒看到|沒看到|沒有看到|沒看見|沒有看見|沒做|没做|沒有做|commit`)
	pathReferenceRe      = regexp.MustCompile(`(?:^|[\s'"(（])(?:\.{1,2}/|/)[^\s'"）)]+|(?:^|[\s'"(（])[\w.-]+(?:/[\w.-]+)+`)
	conceptualQuestionRe = regexp.MustCompile(`(?i)\b(?:what|who)\s+(?:is|are)\b|是什麼|是什么|是啥|解釋一下|解释一下|介紹一下|介绍一下`)
	explicitActionVerbRe = regexp.MustCompile(`(?i)\b(?:write|edit|modify|fix|implement|create|delete|run|execute|test|build|commit|crawl|scrape|search)\b|幫我|修|改|寫|建立|執行|測試|查資料|找資料|爬取|儲存|保存`)
	artifactAdviceRe     = regexp.MustCompile(`(?is)(?:如果|假如|假設|假设|是不是|是否|可不可以|能不能|該不該|该不该|我想(?:知道|了解|測試|测试)|\bif\b|suppose|hypothetically|how\s+(?:would|should|could|can)|what\s+(?:is|would)|would\s+it|should\s+i).{0,180}(?:怎麼|怎么|如何|方式|方法|建議|建议|比較好|比较好|影響|影响|測試|测试|網頁|网页|website|webpage|page|app)`)
	directArtifactAskRe  = regexp.MustCompile(`(?is)(?:幫我|帮我|請你|请你|請幫|请帮|替我|直接|please|can\s+you|could\s+you|i\s+want\s+you\s+to).{0,80}(?:做|製作|制作|建立|創建|创建|開發|开发|寫|写|create|make|build|develop|write|implement)|^(?:做|製作|制作|建立|創建|创建|開發|开发|寫|写|create|make|build|develop|write|implement)\b`)

	systemReminderRe = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`)
	multiBlankRe     = regexp.MustCompile(`\n{3,}`)
	exitCodeErrRe    = regexp.MustCompile(`\((?:exit code|command exited with code) (?:-[0-9]+|[1-9][0-9]*)\)`)

	conversationCapabilityRe = regexp.MustCompile(`(?is)\b(?:what can you do|what are your capabilities|how can you help|how would (?:you|the model) respond|evaluate (?:your|the model(?:'s)?) capabilities?)\b|你(?:能|可以|會|会)幫我做什麼|你(?:能|可以|會|会)做什麼|你有什麼能力|可以怎麼幫我|有哪些能力|會怎麼反應|会怎么反应|如何反應|如何反应|評估.{0,24}(?:能力|表現)|评估.{0,24}(?:能力|表现)`)
	conversationMetaRe       = regexp.MustCompile(`(?is)(?:如果|假如|假設|假设|when|if).{0,100}(?:傳給|传给|交給|交给|send|give).{0,80}(?:模型|model|llm|agent).{0,100}(?:反應|反应|respond|回答)|(?:分析|評估|评估|測試|测试|evaluate|analy[sz]e).{0,80}(?:這段|这段|這則|这则|訊息|信息|文字|prompt|message).{0,80}(?:能力|理解|反應|反应|respond|回答|缺少|不足)`)
	readOnlyExploreRe        = regexp.MustCompile(`(?is)(?:幫我|帮我|請|请|please).{0,32}(?:看一下|查看|盤點|盘点|列出|找出|檢視|检视|inspect|list|show).{0,80}(?:本機|本机|工作區|工作区|資料夾|文件夹|目錄|目录|檔案|文件|專案|项目|workspace|files?|tasks?|work)`)
	planOnlyRe               = regexp.MustCompile(`(?is)(?:先|only|first).{0,80}(?:計畫|计划|步驟|步骤|怎麼|怎么|how|plan).{0,100}(?:不要|先不要|不得|不可|without|do not|don't).{0,48}(?:修改|寫入|写入|執行|执行|動手|edit|write|execute|change)|(?:只|only).{0,32}(?:提出|給我|给我|show|provide).{0,32}(?:計畫|计划|步驟|步骤|plan)`)
)

type TurnMode string

const (
	TurnModeChat    TurnMode = "chat"
	TurnModeExplore TurnMode = "explore"
	TurnModePlan    TurnMode = "plan"
	TurnModeAgent   TurnMode = "agent"
)

// StripSystemReminder removes harness-injected reminder blocks before
// intent classification — they would otherwise trip the keyword regexes.
func StripSystemReminder(text string) string {
	t := systemReminderRe.ReplaceAllString(text, " ")
	return strings.TrimSpace(multiBlankRe.ReplaceAllString(t, "\n\n"))
}

func shouldKeepTools(userText string) bool {
	if len([]rune(userText)) > 220 {
		return true
	}
	return keepToolsRe.MatchString(userText) || pathReferenceRe.MatchString(userText) || HasExplicitWebResearchCue(userText)
}

// IsContextualFollowUp reports short "where is it / you didn't do it"
// follow-ups that need conversation history kept.
func IsContextualFollowUp(userText string) bool {
	t := strings.TrimSpace(userText)
	if t == "" || len([]rune(t)) > 160 {
		return false
	}
	return contextualFollowUpRe.MatchString(t)
}

// ShouldRequireTool reports an action turn: the user asked for a local
// effect, so the model must answer through the tool channel.
func ShouldRequireTool(userText string) bool {
	t := strings.TrimSpace(userText)
	if t == "" {
		return false
	}
	if IsArtifactDiscussionQuestion(t) {
		return false
	}
	if conceptualQuestionRe.MatchString(t) && !explicitActionVerbRe.MatchString(t) && !HasExplicitWebResearchCue(t) {
		return false
	}
	return requireToolRe.MatchString(t) || pathReferenceRe.MatchString(t) || HasExplicitWebResearchCue(t)
}

// IsArtifactDiscussionQuestion separates a hypothetical discussion about an
// artifact from a request to create it. Artifact nouns alone must not force a
// write-file controller or leave the next turn trapped in an unfinished task.
func IsArtifactDiscussionQuestion(userText string) bool {
	t := strings.TrimSpace(StripSystemReminder(userText))
	if t == "" || directArtifactAskRe.MatchString(t) {
		return false
	}
	return artifactAdviceRe.MatchString(t)
}

// ShouldUseMinimalChat reports short casual chat where tools are noise.
func ShouldUseMinimalChat(userText string) bool {
	return !shouldKeepTools(userText) && !IsContextualFollowUp(userText)
}

// ClassifyTurnMode selects a capability boundary, not a solution. The model
// still owns intent understanding, planning, tool choice, and replanning
// inside the selected boundary.
func ClassifyTurnMode(userText string) TurnMode {
	text := StripSystemReminder(userText)
	switch {
	case planOnlyRe.MatchString(text):
		return TurnModePlan
	case conversationCapabilityRe.MatchString(text), conversationMetaRe.MatchString(text):
		return TurnModeChat
	case IsProgressOrContinueFollowUp(text):
		// "進度？" after an unfinished write must keep tools. Chat mode is
		// what made the snake follow-up emit prose instead of write_file.
		return TurnModeAgent
	case readOnlyExploreRe.MatchString(text):
		return TurnModeExplore
	case ShouldRequireTool(text):
		return TurnModeAgent
	default:
		return TurnModeChat
	}
}

// IsProgressOrContinueFollowUp reports a short "how far / keep going"
// turn that is not casual chat.
func IsProgressOrContinueFollowUp(userText string) bool {
	t := strings.TrimSpace(StripSystemReminder(userText))
	if t == "" || len([]rune(t)) > 40 {
		return false
	}
	return progressOrContinueRe.MatchString(t)
}

// --- model profiles ---

type Profile string

const (
	ProfileE2B     Profile = "e2b"
	ProfileE4B     Profile = "e4b"
	Profile12B     Profile = "12b"
	Profile26B     Profile = "26b"
	ProfileQwen    Profile = "qwen"
	ProfileDefault Profile = "default"
)

var (
	e2bRe  = regexp.MustCompile(`(?i)gemma[-_]?4[-_]?e2b|(^|[-_/. ])e2b([-_/. ]|$)`)
	e4bRe  = regexp.MustCompile(`(?i)gemma[-_]?4[-_]?e4b|(^|[-_/. ])e4b([-_/. ]|$)`)
	b12Re  = regexp.MustCompile(`(?i)gemma[-_]?4[-_]?12b|(^|[-_/. ])12b([-_/. ]|$)`)
	b26Re  = regexp.MustCompile(`(?i)gemma[-_]?4[-_]?26b|(^|[-_/. ])26b([-_/. ]|$)|a4b`)
	qwenRe = regexp.MustCompile(`(?i)qwen3|qwen3[._]5|qwen3\.6|qwythos|claude[-_ ]?mythos|meadow[-_ ]?(9b|27b|35b)|(^|[-_/. ])(9b|27b|35b)([-_/. ]|$)`)
)

func ProfileForModel(modelID string) Profile {
	switch {
	case e2bRe.MatchString(modelID):
		return ProfileE2B
	case e4bRe.MatchString(modelID):
		return ProfileE4B
	case b12Re.MatchString(modelID):
		return Profile12B
	case b26Re.MatchString(modelID):
		return Profile26B
	case qwenRe.MatchString(modelID):
		return ProfileQwen
	default:
		return ProfileDefault
	}
}

// --- helpers over provider.Message history ---

func lastUserText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return strings.TrimSpace(provider.MessageText(msgs[i]))
		}
	}
	return ""
}

func lastUserMessage(msgs []provider.Message) (provider.Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i], true
		}
	}
	return provider.Message{}, false
}

func hasAssistantToolCall(msgs []provider.Message, toolName string) bool {
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == toolName {
				return true
			}
		}
	}
	return false
}

func hasAnyAssistantToolCall(msgs []provider.Message, toolNames ...string) bool {
	for _, name := range toolNames {
		if hasAssistantToolCall(msgs, name) {
			return true
		}
	}
	return false
}

func hasToolResult(msgs []provider.Message) bool {
	for _, m := range msgs {
		if m.Role == "tool" {
			return true
		}
	}
	return false
}

func hasSuccessfulToolCall(msgs []provider.Message, names ...string) bool {
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	callIDs := map[string]bool{}
	for _, message := range msgs {
		if message.Role != "assistant" {
			continue
		}
		for _, call := range message.ToolCalls {
			if want[call.Function.Name] {
				callIDs[call.ID] = true
			}
		}
	}
	for _, message := range msgs {
		if message.Role == "tool" && callIDs[message.ToolCallID] && !toolResultIsError(message.Content) {
			return true
		}
	}
	return false
}

func lastToolResultIsError(msgs []provider.Message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case "tool":
			t := strings.TrimSpace(msgs[i].Content)
			return strings.HasPrefix(t, "ERROR:") || exitCodeErrRe.MatchString(t)
		case "user":
			return false
		}
	}
	return false
}

func lastAssistantToolCallIndex(msgs []provider.Message, names ...string) int {
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		for _, tc := range msgs[i].ToolCalls {
			if want[tc.Function.Name] {
				return i
			}
		}
	}
	return -1
}

func lastToolResultText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" && strings.TrimSpace(msgs[i].Content) != "" {
			return strings.TrimSpace(msgs[i].Content)
		}
	}
	return ""
}

func lastToolResultCallName(msgs []provider.Message) string {
	resultID := ""
	resultIndex := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			resultID = msgs[i].ToolCallID
			resultIndex = i
			break
		}
	}
	if resultID == "" {
		return ""
	}
	for i := resultIndex - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		for _, call := range msgs[i].ToolCalls {
			if call.ID == resultID {
				return call.Function.Name
			}
		}
	}
	return ""
}

func lastNamedToolResult(msgs []provider.Message, toolName string) (int, string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		for _, call := range msgs[i].ToolCalls {
			if call.Function.Name != toolName {
				continue
			}
			for j := i + 1; j < len(msgs); j++ {
				if msgs[j].Role == "tool" && msgs[j].ToolCallID == call.ID {
					return i, strings.TrimSpace(msgs[j].Content), true
				}
			}
			return i, "", false
		}
	}
	return -1, "", false
}

func toolResultIsError(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "ERROR:") || exitCodeErrRe.MatchString(content)
}

func recentNonSystem(msgs []provider.Message, keep int) []provider.Message {
	var out []provider.Message
	for i := len(msgs) - 1; i >= 0 && len(out) < keep; i-- {
		if msgs[i].Role == "system" {
			continue
		}
		out = append([]provider.Message{msgs[i]}, out...)
	}
	return out
}

// compactToolResultHistory ports compactLocalGemma4ToolResultHistory: keep
// from the user turn that started the current tool run, drop system
// messages, strip reminders from user content.
func compactToolResultHistory(msgs []provider.Message) []provider.Message {
	firstToolCall := -1
	for i, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			firstToolCall = i
			break
		}
	}
	if firstToolCall < 0 {
		return recentNonSystem(msgs, 8)
	}
	start := firstToolCall
	for i := firstToolCall - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			start = i
			break
		}
	}
	var out []provider.Message
	for _, m := range msgs[start:] {
		if m.Role == "system" {
			continue
		}
		if m.Role == "user" {
			m = withUserText(m, StripSystemReminder(provider.MessageText(m)))
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Tool-call turns should be represented by the structured
			// ToolCalls field. Some Qwen-family models also print a fake
			// transcript in Content; feeding that back teaches the next turn
			// to continue the transcript instead of issuing a real API call.
			m.Content = ""
		}
		out = append(out, m)
	}
	return out
}

// --- per-turn tool staging ---

// staticWebRequiredFiles mirrors STATIC_WEB_REQUIRED_FILES.
var staticWebRequiredFiles = []string{"index.html", "style.css", "script.js"}

func writtenFileBasenames(msgs []provider.Message) map[string]bool {
	written := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != ToolWrite {
				continue
			}
			var args struct {
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil && args.FilePath != "" {
				written[filepath.Base(args.FilePath)] = true
			}
		}
	}
	return written
}

// hasWrittenStandaloneHTML reports a write_file call targeting a .html
// file OUTSIDE the index/style/script triple (e.g. snake_game.html) — the
// model chose the self-contained single-file route, so the staged triple
// no longer applies. An index.html write does NOT count: that is the
// first file of the triple and style.css/script.js must still follow.
func hasWrittenStandaloneHTML(msgs []provider.Message) bool {
	for name := range writtenFileBasenames(msgs) {
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".html") && lower != "index.html" {
			return true
		}
	}
	return false
}

// --- 增量建構（用戶設計）：骨架 → edit 分段填，不要一次寫整檔 ---
//
// 局部模型一次生成裝不下完整遊戲/網站（容量＋KV 記憶體壓力），所以
// static-web 拆兩段：先 write_file 一個小骨架（結構＋空的 style/script
// 區塊，內含明確佔位字串），再 edit 把佔位換成真正的程式。每段都是聚焦
// 的小生成，self-repair 在每次寫入後驗證語法。
//
// 優雅降級：若模型直接一次寫完整檔（佔位字串不存在），stage 判定為「完成」
// ——強模型不受影響，弱模型才走分段。
const (
	ScriptPlaceholder = "/* SCRIPT_HERE */"
	StylePlaceholder  = "/* STYLE_HERE */"
)

// htmlNameRe extracts a filename the user named ("snake.html"); else the
// backend defaults to index.html.
var htmlNameRe = regexp.MustCompile(`[\w.\-]+\.html?\b`)

// taskFileRe matches relative filenames with a conventional extension. Keep
// this language-agnostic so a new code type (.zig, .rs, .sol, …) is still a
// valid deliverable without teaching the harness every language in advance.
var taskFileRe = regexp.MustCompile(`/?(?:[\w.\-]+/)*[\w.\-]+\.[A-Za-z][A-Za-z0-9]{0,11}\b`)

// Target cues are evaluated only in the text between the previous filename
// and the current one. This prevents "create report.html using data.csv" from
// incorrectly applying create to data.csv as well.
var (
	targetCreateCueRe           = regexp.MustCompile(`(?is)(?:做成|做出|做一?個|建立|建一?個|完成|實作|存成|存為|存到|存於|輸出|匯出|儲存|寫成|寫出|寫到|寫一?個|生成|產出|產生|save\s+(?:as|to|it)?|output|write\s+(?:to|out|it)?|create|produce|generate|make|build|implement)[^.!?\n。！？]{0,64}$`)
	targetModifyCueRe           = regexp.MustCompile(`(?is)(?:修正|修復|修改|更新|調整|改善|處理|解決|fix|repair|modify|update|edit|change|patch|improve|resolve)[^.!?\n。！？]{0,64}$`)
	targetReferenceCueRe        = regexp.MustCompile(`(?is)(?:讀取|查看|檢查|參考|依照|根據|載入|執行|跑|read|inspect|open|load|from|using|with|follow|according\s+to|documented\s+in|run|execute)[^.!?\n。！？]{0,64}$`)
	targetForbiddenCueRe        = regexp.MustCompile(`(?is)(?:不要|不得|禁止|勿|do\s+not|don't|never)[^.!?\n。！？]{0,40}(?:修改|更改|動|改|modify|edit|change)[^.!?\n。！？]{0,40}$`)
	targetObligationRe          = regexp.MustCompile(`(?is)^\s*(?:的[^.!?\n。！？]{0,64}(?:為|是|需|應|必須)|(?:must|should|shall|required)\b|(?:需|應|必須)(?:包含|具有|為|是))`)
	exclusiveMutationScopeCueRe = regexp.MustCompile(`(?is)(?:只(?:允許|能|可)?(?:修改|更改|編輯|修正|修復)|僅(?:允許)?(?:修改|更改|編輯|修正|修復)|(?:may|can|must)?\s*only\s+(?:modify|edit|change|fix|repair)|(?:modify|edit|change|fix|repair)\s+only)`)
	mutationScopeForbiddenRe    = regexp.MustCompile(`(?is)(?:不要|不得|禁止|勿|do\s+not|don't|never)`)
)

// producedExtRe marks extensions that are almost always a deliverable the model
// writes (vs a data source it reads, like .csv/.json).
var producedExtRe = regexp.MustCompile(`(?i)\.(?:html?|css|js|mjs|svg|md)$`)

// TaskTargetFile returns the relative artifact file the task wants
// PRODUCED, or "" when none is named. Lets the harness save/backfill the file
// the user actually asked for. When the task names several files (e.g. "read
// sales.csv, make report.html"), it must return the OUTPUT (report.html), not
// the first-named input (sales.csv) — otherwise the backfill nudge aims at a
// file that already exists and never fires.
func TaskTargetFile(userText string) string {
	t := StripSystemReminder(userText)
	// A delegated task/spec file is an input contract, not the deliverable.
	// The real output path is learned after read_file; guessing here risks
	// backfilling or validating the protected instruction file itself.
	if isDelegatedLocalTask(t) {
		return ""
	}
	validationFiles := map[string]bool{}
	if command := NamedValidationCommand(t); command != "" {
		for _, name := range taskFileRe.FindAllString(command, -1) {
			validationFiles[filepath.Clean(name)] = true
			validationFiles[filepath.Base(name)] = true
		}
	}
	rawLocs := taskFileRe.FindAllStringIndex(t, -1)
	var locs [][]int
	previousRawEnd := 0
	for _, loc := range rawLocs {
		name := t[loc[0]:loc[1]]
		prefix := t[previousRawEnd:loc[0]]
		previousRawEnd = loc[1]
		validationMention := validationFiles[filepath.Clean(name)] || validationFiles[filepath.Base(name)]
		// A requested output often appears again inside its validator (for
		// example "create report.md, then run `test -s report.md`"). Keep the
		// explicit create/modify mention while still excluding validator-only
		// filenames such as test modules and checker scripts.
		explicitOutput := targetCreateCueRe.MatchString(prefix) || targetModifyCueRe.MatchString(prefix)
		if isLibraryFileMention(name) || isLikelyQualifiedSymbol(name) ||
			(validationMention && !explicitOutput) {
			continue
		}
		locs = append(locs, loc)
	}
	if len(locs) == 0 {
		return ""
	}
	cleanTarget := func(name string) string {
		clean := filepath.Clean(strings.TrimSpace(name))
		if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
			return filepath.Base(name)
		}
		return clean
	}
	bestName, bestScore := "", 19 // require an actual create/modify intent
	previousEnd := 0
	for index, loc := range locs {
		name := t[loc[0]:loc[1]]
		prefix := t[previousEnd:loc[0]]
		score := index // deterministic tie-break: later explicit outputs win
		if targetCreateCueRe.MatchString(prefix) {
			score += 100
		}
		if targetModifyCueRe.MatchString(prefix) {
			score += 80
		}
		if targetReferenceCueRe.MatchString(prefix) {
			score -= 40
		}
		if targetForbiddenCueRe.MatchString(prefix) {
			score -= 160
		}
		base := strings.ToLower(filepath.Base(name))
		if strings.HasPrefix(base, "readme.") {
			score -= 25
		}
		if strings.Contains(strings.ToLower(name), "test") {
			score -= 10
		}
		if producedExtRe.MatchString(name) {
			score += 3
		}
		if score > bestScore {
			bestName, bestScore = name, score
		}
		previousEnd = loc[1]
	}
	if bestName == "" {
		return ""
	}
	return cleanTarget(bestName)
}

// TaskTargetFiles returns every explicitly requested output path, rather than
// only the single best target used by legacy backfill behavior. This lets the
// kernel completion gate track contracts such as "create report.md and
// answer.json" without treating referenced inputs or validator files as
// deliverables. It deliberately extracts paths only; it does not infer task
// semantics or expected file contents.
func TaskTargetFiles(userText string) []string {
	t := StripSystemReminder(userText)
	locs := taskFileRe.FindAllStringIndex(t, -1)
	if len(locs) == 0 {
		return nil
	}
	cleanTarget := func(name string) string {
		clean := filepath.Clean(strings.TrimSpace(name))
		if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
			return filepath.Base(name)
		}
		return clean
	}
	seen := map[string]bool{}
	targets := make([]string, 0, len(locs))
	previousEnd := 0
	for index, loc := range locs {
		name := t[loc[0]:loc[1]]
		prefix := t[previousEnd:loc[0]]
		nextStart := len(t)
		if index+1 < len(locs) {
			nextStart = locs[index+1][0]
		}
		suffix := t[loc[1]:nextStart]
		previousEnd = loc[1]
		if isLibraryFileMention(name) || isLikelyQualifiedSymbol(name) {
			continue
		}
		explicitOutput := targetCreateCueRe.MatchString(prefix) ||
			targetModifyCueRe.MatchString(prefix) ||
			targetObligationRe.MatchString(suffix)
		if !explicitOutput {
			continue
		}
		if targetReferenceCueRe.MatchString(prefix) &&
			!targetCreateCueRe.MatchString(prefix) &&
			!targetModifyCueRe.MatchString(prefix) {
			continue
		}
		if targetForbiddenCueRe.MatchString(prefix) {
			continue
		}
		target := cleanTarget(name)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		targets = append(targets, target)
	}
	return targets
}

// TaskDeliverableFiles returns user-authored output paths whose exact existence
// may be used as a completion gate. It deliberately does not invent a filename
// for an unnamed artifact: the model may reasonably choose snake_game.html
// instead of index.html. Unnamed creation is still protected by the task
// controller's native-mutation requirement and the evidence ledger's observed
// ChangedPaths, without turning an arbitrary default name into user intent.
func TaskDeliverableFiles(userText string) []string {
	targets := TaskTargetFiles(userText)
	if len(targets) == 0 {
		if target := TaskTargetFile(userText); target != "" {
			targets = []string{target}
		}
	}
	if len(targets) > 0 {
		return targets
	}
	return nil
}

// TaskExplicitMutationScope returns the exact file paths from an explicit
// user-authored exclusive mutation clause such as "只允許修改 src/task.py" or
// "only modify src/task.py and config.json". It intentionally returns nil for
// ordinary create/modify requests: an allowlist is a hard execution boundary,
// so the router must never infer one from a non-exclusive task description.
func TaskExplicitMutationScope(userText string) []string {
	t := StripSystemReminder(userText)
	cueLocs := exclusiveMutationScopeCueRe.FindAllStringIndex(t, -1)
	if len(cueLocs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var paths []string
	for _, cue := range cueLocs {
		clause := explicitMutationScopeClause(t[cue[1]:])
		for _, raw := range taskFileRe.FindAllString(clause, -1) {
			clean := filepath.Clean(strings.TrimSpace(raw))
			if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				continue
			}
			if !seen[clean] {
				seen[clean] = true
				paths = append(paths, clean)
			}
		}
	}
	return paths
}

func explicitMutationScopeClause(tail string) string {
	fileLocs := taskFileRe.FindAllStringIndex(tail, -1)
	insideFile := func(index int) bool {
		for _, loc := range fileLocs {
			if loc[0] <= index && index < loc[1] {
				return true
			}
		}
		return false
	}
	end := len(tail)
	for index, char := range tail {
		if insideFile(index) {
			continue
		}
		if strings.ContainsRune(".!?\n。！？；;", char) {
			end = index
			break
		}
	}
	clause := tail[:end]
	if forbidden := mutationScopeForbiddenRe.FindStringIndex(clause); forbidden != nil {
		clause = clause[:forbidden[0]]
	}
	return clause
}

var libraryFileMentions = map[string]bool{
	"three.js":  true,
	"react.js":  true,
	"vue.js":    true,
	"d3.js":     true,
	"p5.js":     true,
	"gsap.js":   true,
	"matter.js": true,
}

func isLibraryFileMention(name string) bool {
	return libraryFileMentions[strings.ToLower(filepath.Base(strings.TrimSpace(name)))]
}

// isLikelyQualifiedSymbol excludes Go/Rust/Java-style qualified identifiers
// such as cache.Merge or client.Do from the filename extractor. Their suffix
// starts with an uppercase exported symbol, whereas conventional source and
// artifact extensions are overwhelmingly lowercase (README.MD remains a
// filename because both sides are uppercase). Without this guard, a request to
// "fix cache.Merge" becomes a bogus exact deliverable and the completion gate
// coerces the agent into creating a file literally named cache.Merge.
func isLikelyQualifiedSymbol(name string) bool {
	base := filepath.Base(strings.TrimSpace(name))
	if strings.ContainsAny(strings.TrimSpace(name), `/\`) {
		return false
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot+1 >= len(base) {
		return false
	}
	left, right := base[:dot], base[dot+1:]
	first := right[0]
	return first >= 'A' && first <= 'Z' && left != strings.ToUpper(left)
}

// computeNeedRe matches a task that asks the model to PRODUCE a computed number
// (a total, sum, average, aggregate) — where a weak model's mental arithmetic is
// unreliable (observed: 12B got accounting totals wrong; 26B wrote Python and was
// right). calcAppRe excludes "build a calculator app", which computes at runtime
// in the generated code, not at build time by the model.
var (
	computeNeedRe          = regexp.MustCompile(`(?i)總額|總計|總和|合計|小計|加總|彙總|統計|平均|sum\b|subtotal|grand[ -]?total|aggregate|計算.{0,10}(總|金額|數量|每|票數|件數|筆數|數目|比例|優先|sla|違約|通過|失敗)`)
	calcAppRe              = regexp.MustCompile(`(?i)計算機|計算器|calculator`)
	executableValidationRe = regexp.MustCompile(`(?i)(?:\b(?:executable|runnable)\s+(?:program|script|checker)\b.{0,80}\b(?:validate|verify|check|acceptance)\b|\b(?:run|execute)\b.{0,40}\b(?:calculation|validation|verification|checker)\b|(?:可執行|可执行)(?:程式|程序|腳本|脚本).{0,48}(?:驗證|验证|檢查|检查|驗收|验收)|(?:實際|实际)(?:執行|执行).{0,20}(?:計算|计算|驗證|验证|檢查|检查|驗收|验收))`)
)

// NeedsComputation reports whether the task wants the model to compute a result
// from given data — a signal to compute via a tool (run_shell_command + Python)
// instead of mental arithmetic.
func NeedsComputation(userText string) bool {
	t := StripSystemReminder(userText)
	if calcAppRe.MatchString(t) {
		return false
	}
	return computeNeedRe.MatchString(t)
}

// TaskRequiresExecutableValidation identifies an explicit acceptance-program
// obligation rather than merely a request to create a file. Numeric tasks also
// qualify because their computed claims need a falsifiable check instead of a
// prose assertion. The harness chooses only the evidence capability; the model
// still authors the checker and expected relationships.
func TaskRequiresExecutableValidation(userText string) bool {
	t := StripSystemReminder(userText)
	return NeedsComputation(t) || executableValidationRe.MatchString(t)
}

// StaticWebScaffold returns the filename and skeleton the Go backend
// pre-creates for a static-web build. The model then only edits to fill the
// placeholders: it spends no tokens on the path or structure and can never
// corrupt a filename it does not write. Derives the name from the request
// when the user named a file, else index.html.
func StaticWebScaffold(userText string) (string, string) {
	name := "index.html"
	if m := htmlNameRe.FindString(StripSystemReminder(userText)); m != "" {
		name = filepath.Base(m)
	}
	skeleton := "<!DOCTYPE html>\n<html lang=\"zh-TW\">\n<head>\n" +
		"<meta charset=\"UTF-8\">\n" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n" +
		"<title>App</title>\n<style>\n" + StylePlaceholder + "\n</style>\n</head>\n" +
		"<body>\n<canvas id=\"game\" width=\"400\" height=\"400\"></canvas>\n" +
		"<script>\n" + ScriptPlaceholder + "\n</script>\n</body>\n</html>\n"
	return name, skeleton
}

// staticWebTarget returns the .html file the session is building — the most
// recent write_file/edit targeting a .html, or "" when none yet.
func staticWebTarget(msgs []provider.Message) string {
	target := ""
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != ToolWrite && tc.Function.Name != ToolEdit {
				continue
			}
			var a struct {
				FilePath string `json:"file_path"`
				Path     string `json:"path"`
			}
			if json.Unmarshal([]byte(tc.Function.Arguments), &a) != nil {
				continue
			}
			p := a.FilePath
			if p == "" {
				p = a.Path
			}
			if strings.HasSuffix(strings.ToLower(p), ".html") ||
				strings.HasSuffix(strings.ToLower(p), ".htm") {
				target = filepath.Base(p)
			}
		}
	}
	return target
}

// staticWebStage reports whether the static-web build still needs a write:
//
//	1 = 還沒有實質的單檔（下一步 write_file 寫完整自包含 .html）
//	0 = 完成（磁碟上有夠大且含 inline <script> 的 .html）
//
// One-shot by design: the model writes the whole self-contained file in a
// single call so its HTML and JS stay internally consistent (a fixed
// skeleton + edit-fill breaks that — see the gated MEADOW_SCAFFOLD path).
// Self-repair validates the result; a husk (< 1500 bytes or no inline
// script) keeps stage 1 so the model is nudged to write a real file.
func staticWebStage(msgs []provider.Message, cwd string) (int, string) {
	target := staticWebTarget(msgs)
	if target == "" && cwd != "" {
		target = discoverStaticWebTarget(cwd)
	}
	if target == "" {
		return 1, ""
	}
	if cwd == "" {
		return 0, target
	}
	b, err := os.ReadFile(filepath.Join(cwd, target))
	if err != nil {
		return 1, target
	}
	if len(b) > 1500 && strings.Contains(strings.ToLower(string(b)), "<script") {
		return 0, target // 實質單檔已交付
	}
	return 1, target
}

// discoverStaticWebTarget recovers an artifact after Desktop history
// compaction removed the originating write_file metadata. Only an unambiguous
// HTML candidate is selected; index.html wins by convention.
func discoverStaticWebTarget(cwd string) string {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return ""
	}
	candidate := ""
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".html" && ext != ".htm" {
			continue
		}
		if strings.EqualFold(name, "index.html") {
			return name
		}
		if candidate != "" {
			return ""
		}
		candidate = name
	}
	return candidate
}

// UnfinishedStagedFiles reports whether a static-web build is still in
// progress (skeleton missing, or script placeholder not yet filled). The
// engine uses it to refuse a text-only finish: a model that prints code in
// chat instead of writing/editing must be nudged back to the tool.
func UnfinishedStagedFiles(userText string, msgs []provider.Message, cwd string) []string {
	plan := Classify(StripSystemReminder(userText))
	if plan == nil || plan.ID != IntentStaticWebArtifact {
		return nil
	}
	stage, target := staticWebStage(msgs, cwd)
	if stage == 0 {
		return nil
	}
	if target == "" {
		target = "index.html"
	}
	return []string{target}
}

var (
	fencedBlockRe = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\\n(.*?)\\n?```")

	// HarnessCommand patterns — only the clearly-determinable, SAFE shell
	// ops. Destructive (rm/mv-overwrite) and anything needing judgement is
	// deliberately excluded so direct execution is always safe.
	dirNameNamedRe = regexp.MustCompile("(?i)(?:named|called|叫做|叫|名為|名叫)\\s*[\"'`]?([A-Za-z0-9][A-Za-z0-9_./-]*)")
	dirNameKwRe    = regexp.MustCompile("(?i)(?:directory|folder|dir)\\s+[\"'`]?([A-Za-z0-9][A-Za-z0-9_./-]*)")
	dirNameCjkRe   = regexp.MustCompile("([A-Za-z0-9][A-Za-z0-9_./-]*)\\s*(?:的)?\\s*(?:資料夾|文件夾|目錄)")
	pureListRe     = regexp.MustCompile("(?i)^\\s*(?:list|ls|show(?: me)?)\\b[^.]*\\b(?:files?|directory|dir|contents?)\\b|列出.{0,6}(?:檔案|文件|目錄|清單)|(?:這|此)(?:個|裡|裏|資料夾)?[^,。]*有(?:哪些|什麼)(?:檔案|文件)|what files are in")
	listVetoRe     = regexp.MustCompile("(?i)\\b(find|search|grep|create|write|delete|edit|fix|run)\\b|搜尋|搜索|尋找|查找|建立|刪|修")
	negClauseRe    = regexp.MustCompile("(?i)(?:do ?not|don'?t|不要|不可|別|勿|沒有要|不需要)\\s*\\S+")
	// A conjunction means more than one action — never short-circuit a
	// compound request (e.g. "mkdir assets AND put a README in it") or the
	// harness would silently drop everything after the first verb.
	compoundRe  = regexp.MustCompile("(?i)\\b(?:and|then|also|after that)\\b|然後|接著|並(?:且)?|以及|還要|再(?:來|把|建|寫|加)|，.*[建寫加做執跑]")
	gitStatusRe = regexp.MustCompile("(?i)\\bgit status\\b|what(?:'?s| has) changed|有(?:哪些|什麼)?(?:改動|變更|變動)|目前.{0,4}git.{0,4}狀態")
	dirNameStop = map[string]bool{"a": true, "an": true, "the": true, "new": true, "this": true, "my": true, "one": true, "named": true, "called": true}
)

// HarnessCommand returns a shell command the harness can run DIRECTLY for a
// high-confidence, fully-determinable, SAFE intent (mkdir, list, git status) —
// skipping the model entirely. ok is false for anything ambiguous, anything
// needing model judgement, or anything destructive, so the caller falls
// through to the normal model-driven loop. This is the harness-driven half of
// "Bash as a universal terminal": the harness composes and runs the command,
// the weak model never sees it (no drift, no prompt cost, instant).
func HarnessCommand(userText string) (string, bool) {
	t := strings.TrimSpace(StripSystemReminder(userText))
	if t == "" {
		return "", false
	}
	// Compound request → let the model drive (don't drop later steps).
	if compoundRe.MatchString(t) {
		return "", false
	}
	// mkdir — only when the router itself classified directory_create.
	if plan := Classify(t); plan != nil && plan.ID == IntentDirectoryCreate {
		if name := extractDirName(t); name != "" {
			return "mkdir -p " + name, true
		}
	}
	// pure directory listing (read-only) — not a content search. Strip
	// negated clauses first so "list files, do NOT create anything" isn't
	// vetoed by the "create" inside the negation.
	if pureListRe.MatchString(t) && !listVetoRe.MatchString(negClauseRe.ReplaceAllString(t, " ")) {
		return "ls -la", true
	}
	// git status (read-only).
	if gitStatusRe.MatchString(t) {
		return "git status", true
	}
	return "", false
}

func extractDirName(t string) string {
	for _, re := range []*regexp.Regexp{dirNameNamedRe, dirNameKwRe, dirNameCjkRe} {
		if m := re.FindStringSubmatch(t); m != nil {
			name := m[1]
			if !dirNameStop[strings.ToLower(name)] && safeHarnessDirectoryName(name) {
				return name
			}
		}
	}
	return ""
}

func safeHarnessDirectoryName(name string) bool {
	name = strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	if name == "" || strings.HasPrefix(name, "/") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

// ExtractArtifact pulls a deliverable file body out of a model response that
// printed/described the artifact as prose instead of calling write_file — the
// dominant failure at high temperature, where the model drifts away from
// structured tool calls. It returns the largest fenced code block, or the
// whole response when that is itself a full HTML document. ok is false when
// nothing substantial is found, so the caller can fall back to nudging.
//
// This is the "harness handles the mechanics" half of the decoupling: the
// model only has to PRODUCE the content; the harness, not the model's
// tool-call compliance, performs the write.
func ExtractArtifact(content string) (string, bool) {
	best := ""
	for _, m := range fencedBlockRe.FindAllStringSubmatch(content, -1) {
		if len(m) > 1 && len(m[1]) > len(best) {
			best = m[1]
		}
	}
	if len(strings.TrimSpace(best)) >= 200 {
		return best, true
	}
	trimmed := strings.TrimSpace(content)
	low := strings.ToLower(trimmed)
	if (strings.Contains(low, "<!doctype") || strings.HasPrefix(low, "<html")) &&
		strings.Contains(low, "</html>") && len(trimmed) >= 200 {
		return trimmed, true
	}
	return "", false
}

func missingStaticWebFiles(msgs []provider.Message, cwd string) []string {
	written := writtenFileBasenames(msgs)
	if cwd != "" {
		// 單檔自包含交付：index.html 帶 inline <script> 且有實質內容，
		// 三件套即視為滿足——26B 在單檔路線穩定，分檔路線會交空殼
		//（live 實測 script.js 69 bytes），所以 stage 指令也偏向單檔。
		if b, err := os.ReadFile(filepath.Join(cwd, "index.html")); err == nil {
			if len(b) > 1500 && strings.Contains(string(b), "<script>") {
				return nil
			}
		}
		for _, name := range staticWebRequiredFiles {
			if _, err := os.Stat(filepath.Join(cwd, name)); err == nil {
				written[name] = true
			}
		}
	}
	var missing []string
	for _, name := range staticWebRequiredFiles {
		if !written[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func firstAvailable(desired []string, selected []string) []string {
	available := map[string]bool{}
	for _, n := range selected {
		available[n] = true
	}
	for _, n := range desired {
		if available[n] {
			return []string{n}
		}
	}
	return append([]string{}, selected...)
}

// bothAvailable keeps every desired tool that survives the availability
// filter (vs firstAvailable's single pick); falls back to all selected.
func bothAvailable(desired []string, selected []string) []string {
	available := map[string]bool{}
	for _, n := range selected {
		available[n] = true
	}
	var out []string
	for _, n := range desired {
		if available[n] {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return append([]string{}, selected...)
	}
	return out
}

func selectedHas(selected []string, name string) bool {
	for _, n := range selected {
		if n == name {
			return true
		}
	}
	return false
}

// SelectTurnTools ports selectLocalGemma4TurnToolNames: narrow the plan's
// toolset to the single next staged tool, based on which calls already
// happened.
//
// One deliberate divergence from TS: when the latest tool result is an
// error, the stage lock is released and the model gets the plan's full
// toolset. A forced single tool plus a failing tool is an unrecoverable
// loop (observed live: write_file rejected by the prior-read guard while
// tool_choice stayed pinned to write_file).
func SelectTurnTools(plan *Plan, selected []string, msgs []provider.Message, cwd string) []string {
	if plan == nil {
		return append([]string{}, selected...)
	}
	if lastToolResultIsError(msgs) {
		switch plan.ID {
		case IntentEngineeringFix:
			if strings.Contains(lastToolResultText(msgs), "wrong validation command") {
				return bothAvailable([]string{ToolRead, ToolEdit, ToolWrite, ToolShell}, selected)
			}
			if strings.Contains(lastToolResultText(msgs), "no such file") ||
				strings.Contains(lastToolResultText(msgs), "stat ") {
				if name := lastToolResultCallName(msgs); name == ToolRead || name == ToolEdit || name == ToolWrite {
					return firstAvailable([]string{ToolGrep, ToolGlob, ToolList}, selected)
				}
				return bothAvailable([]string{ToolList, ToolGlob, ToolGrep, ToolRead}, selected)
			}
			return bothAvailable([]string{ToolList, ToolGlob, ToolGrep, ToolRead, ToolEdit, ToolWrite}, selected)
		case IntentStaticWebArtifact:
			if strings.Contains(strings.ToLower(lastToolResultText(msgs)), "must be read before overwrite") {
				return firstAvailable([]string{ToolRead}, selected)
			}
			return bothAvailable([]string{ToolRead, ToolEdit, ToolWrite}, selected)
		case IntentResearchArtifact:
			return bothAvailable([]string{ToolWebSearch, ToolWebCrawl, ToolWebFetch, ToolBrowsePage, ToolDownload, ToolResearch}, selected)
		}
		return append([]string{}, selected...)
	}
	switch plan.ID {
	case IntentDelegatedLocalTask:
		if !hasSuccessfulToolCall(msgs, ToolRead) {
			return firstAvailable([]string{ToolRead}, selected)
		}
		return bothAvailable(
			[]string{ToolWrite, ToolEdit, ToolShell, ToolRead, ToolGrep, ToolGlob, ToolList},
			selected,
		)
	case IntentSimpleAppend, IntentDirectoryCreate:
		if !hasAssistantToolCall(msgs, ToolShell) {
			return firstAvailable([]string{ToolShell}, selected)
		}
	case IntentSimpleCopy:
		if !hasSuccessfulToolCall(msgs, ToolRead) {
			return firstAvailable([]string{ToolRead}, selected)
		}
		if !hasAssistantToolCall(msgs, ToolWrite) {
			return firstAvailable([]string{ToolWrite}, selected)
		}
	case IntentOfficeArtifact:
		if !hasSuccessfulToolCall(msgs, ToolRead) {
			return firstAvailable([]string{ToolRead}, selected)
		}
		if !hasAssistantToolCall(msgs, ToolWrite) {
			return firstAvailable([]string{ToolWrite}, selected)
		}
		if !hasAssistantToolCall(msgs, ToolShell) {
			return firstAvailable([]string{ToolShell}, selected)
		}
	case IntentEngineeringFix:
		userText := lastUserText(msgs)
		if !hasAnyAssistantToolCall(msgs, plan.Tools...) && validationFirstRe.MatchString(userText) && NamedValidationCommand(userText) != "" {
			return firstAvailable([]string{ToolShell}, selected)
		}
		if !hasAnyAssistantToolCall(msgs, plan.Tools...) {
			if target := TaskTargetFile(userText); target != "" && cwd != "" {
				path := target
				if !filepath.IsAbs(path) {
					path = filepath.Join(cwd, path)
				}
				if info, err := os.Stat(path); err == nil && !info.IsDir() {
					return firstAvailable([]string{ToolRead}, selected)
				}
			}
			// An issue can quote reporter-app paths or traceback frames that do
			// not exist in the checked-out library. Start unfamiliar repairs with
			// semantic search; only a locally verified named file may skip it.
			return firstAvailable([]string{ToolGrep}, selected)
		}
		if !hasSuccessfulToolCall(msgs, ToolRead) {
			// Discovery is a state machine, not an open-ended menu. E2B will
			// otherwise repeat the same too-specific grep after a clean zero-match
			// result. Move it to a broad glob, then pin the next turn to read_file.
			if hasAssistantToolCall(msgs, ToolGlob) {
				return firstAvailable([]string{ToolRead}, selected)
			}
			if hasAssistantToolCall(msgs, ToolGrep) {
				if strings.HasPrefix(lastToolResultText(msgs), "No matches for ") {
					if next := firstAvailable([]string{ToolGlob}, selected); len(next) > 0 {
						return next
					}
				}
				return firstAvailable([]string{ToolRead}, selected)
			}
			if hasAssistantToolCall(msgs, ToolList) {
				if next := firstAvailable([]string{ToolGlob}, selected); len(next) > 0 {
					return next
				}
				return firstAvailable([]string{ToolRead}, selected)
			}
			return bothAvailable([]string{ToolList, ToolGlob, ToolGrep, ToolRead}, selected)
		}
		if !hasSuccessfulToolCall(msgs, ToolEdit, ToolWrite) {
			// Divergence from TS (which pins edit alone): offer both edit and
			// write_file. The 26B goes silent when pinned to bare edit —
			// whole-file rewrite is its reliable repair mode.
			return bothAvailable([]string{ToolEdit, ToolWrite}, selected)
		}
		if !hasAssistantToolCall(msgs, ToolShell) {
			return firstAvailable([]string{ToolShell}, selected)
		}
	case IntentShellScript, IntentConfigArtifact:
		if !hasAssistantToolCall(msgs, ToolWrite) {
			return firstAvailable([]string{ToolWrite}, selected)
		}
		if !hasAssistantToolCall(msgs, ToolShell) {
			return firstAvailable([]string{ToolShell}, selected)
		}
	case IntentDeleteGenerated:
		if !hasAssistantToolCall(msgs, ToolList) {
			return firstAvailable([]string{ToolList}, selected)
		}
		if !hasAssistantToolCall(msgs, ToolShell) {
			return firstAvailable([]string{ToolShell}, selected)
		}
	case IntentStaticWebArtifact:
		// A resumed task may already have a valid HTML artifact on disk even if
		// persisted chat history no longer contains the original write call.
		if stage, target := staticWebStage(msgs, cwd); stage == 1 {
			if target != "" {
				if !hasSuccessfulToolCall(msgs, ToolRead) {
					return firstAvailable([]string{ToolRead}, selected)
				}
				return bothAvailable([]string{ToolEdit, ToolWrite}, selected)
			}
			return firstAvailable([]string{ToolWrite}, selected)
		}
		if NamedValidationCommand(lastUserText(msgs)) != "" && !hasAssistantToolCall(msgs, ToolShell) {
			return firstAvailable([]string{ToolShell}, selected)
		}
		if !hasSuccessfulToolCall(msgs, ToolVisual) && selectedHas(selected, ToolVisual) {
			return []string{ToolVisual}
		}
		if !hasAssistantToolCall(msgs, ToolShell) {
			// Fallback for registries that do not expose Meadow Browser visual
			// checking yet. This should be uncommon in the Go desktop product.
			return firstAvailable([]string{ToolShell}, selected)
		}
	case IntentResearchArtifact:
		if !hasAnyAssistantToolCall(msgs, ToolWebSearch, ToolWebCrawl, ToolWebFetch, ToolBrowsePage) {
			if !hasAssistantToolCall(msgs, ToolAskUser) {
				return bothAvailable([]string{ToolAskUser, ToolWebSearch}, selected)
			}
			return firstAvailable([]string{ToolWebSearch, ToolWebCrawl, ToolWebFetch, ToolBrowsePage}, selected)
		}
		if !hasAssistantToolCall(msgs, ToolResearch) {
			if !hasAssistantToolCall(msgs, ToolDownload) {
				return bothAvailable([]string{ToolDownload, ToolResearch}, selected)
			}
			return firstAvailable([]string{ToolResearch}, selected)
		}
	}
	return append([]string{}, selected...)
}

// IsActionComplete ports isLocalGemma4ActionComplete: the plan's terminal
// tool has run and produced a result, so the next turn is the summary.
func IsActionComplete(plan *Plan, msgs []provider.Message) bool {
	if plan == nil {
		return false
	}
	if lastToolResultIsError(msgs) {
		return false
	}
	if plan.ID == IntentSimpleCopy {
		return hasSuccessfulToolCall(msgs, ToolRead) && hasSuccessfulToolCall(msgs, ToolWrite)
	}
	if plan.ID == IntentInternetResearch {
		return hasToolResult(msgs) &&
			(hasAssistantToolCall(msgs, ToolWebCrawl) ||
				hasAssistantToolCall(msgs, ToolWebSearch) ||
				hasAssistantToolCall(msgs, ToolWebFetch) ||
				hasAssistantToolCall(msgs, ToolBrowsePage))
	}
	if plan.ID == IntentResearchArtifact {
		return hasAssistantToolCall(msgs, ToolResearch) && hasToolResult(msgs)
	}
	if plan.ID == IntentSimpleWrite {
		return hasAssistantToolCall(msgs, ToolWrite) && hasToolResult(msgs)
	}
	if plan.ID == IntentStaticWebArtifact {
		if NamedValidationCommand(lastUserText(msgs)) != "" && !hasAssistantToolCall(msgs, ToolShell) {
			return false
		}
		// Deterministic browser diagnostics are the terminal technical gate.
		// Model-visible still-image evidence is opt-in for an explicit visual ask.
		return hasSuccessfulToolCall(msgs, ToolVisual)
	}
	if plan.ID == IntentEngineeringFix {
		mutateIdx := lastAssistantToolCallIndex(msgs, ToolEdit, ToolWrite)
		shellIdx, shellResult, hasShellResult := lastNamedToolResult(msgs, ToolShell)
		lastIdx := lastAssistantToolCallIndex(msgs, plan.Tools...)
		return hasSuccessfulToolCall(msgs, ToolRead) && mutateIdx >= 0 && shellIdx > mutateIdx && shellIdx == lastIdx && hasShellResult && !toolResultIsError(shellResult)
	}
	if plan.ID == IntentDelegatedLocalTask {
		if !hasSuccessfulToolCall(msgs, ToolRead) {
			return false
		}
		mutateIdx := lastAssistantToolCallIndex(msgs, ToolEdit, ToolWrite)
		shellIdx, shellResult, hasShellResult := lastNamedToolResult(msgs, ToolShell)
		lastIdx := lastAssistantToolCallIndex(msgs, plan.Tools...)
		return mutateIdx >= 0 && shellIdx > mutateIdx && shellIdx == lastIdx &&
			hasShellResult && !toolResultIsError(shellResult)
	}
	if !hasAssistantToolCall(msgs, ToolShell) {
		return false
	}
	return hasToolResult(msgs)
}

// --- prompt builders ---

var validationCommandRe = regexp.MustCompile(`(?i)(?:\brun\s+|\bexecute\s+|執行\s*|执行\s*|跑\s*)(?:exactly\s*)?((?:node|npm|pnpm|yarn|npx|python3?|pytest|bash|sh|vitest|jest|tsc|bun|deno)\b[^\n\r。；;，,]*)`)
var validationBacktickCommandRe = regexp.MustCompile("(?i)(?:\\brun\\s+|\\bexecute\\s+|執行\\s*|执行\\s*|跑\\s*)(?:exactly\\s*)?`([^`]+)`")
var validationFixedBacktickCommandRe = regexp.MustCompile("(?i)(?:validation\\s+command|acceptance\\s+command|驗收命令|验证命令)\\s*(?:is|must\\s+be|is\\s+fixed\\s+to|fixed\\s+to|[:：=]|固定為|固定为|為|为)\\s*`([^`]+)`")
var validationTrailerRe = regexp.MustCompile(`(?i)\s+(?:and|then)\s+(?:tell|show|report|summari[sz]e|explain)\b.*$`)
var validationConditionalTrailerRe = regexp.MustCompile(`(?i)\.\s+(?:if|when)\b.*$`)
var validationSentenceTrailerRe = regexp.MustCompile(`(?i)\.\s+(?:do|please|never|then|after|before|the|this|it|tests?)\b.*$`)
var validationFirstRe = regexp.MustCompile(`(?i)\b(?:first|before\s+(?:editing|changing|repairing)|start\s+by)\b[^\n\r。；;]{0,80}\b(?:run|execute)\b|先(?:執行|执行|跑)|首先(?:執行|执行|跑)`)
var sentenceTailRe = regexp.MustCompile(`[。.!?]+$`)

// NamedValidationCommand extracts the exact "run <cmd>" the user named, so
// the validation stage can demand that command verbatim.
func NamedValidationCommand(userText string) string {
	if fixed := validationFixedBacktickCommandRe.FindAllStringSubmatch(userText, -1); len(fixed) > 0 {
		return strings.TrimSpace(fixed[len(fixed)-1][1])
	}
	if backticks := validationBacktickCommandRe.FindAllStringSubmatch(userText, -1); len(backticks) > 0 {
		return strings.TrimSpace(backticks[len(backticks)-1][1])
	}
	matches := validationCommandRe.FindAllStringSubmatch(userText, -1)
	if len(matches) == 0 {
		return ""
	}
	raw := strings.TrimSpace(matches[len(matches)-1][1])
	raw = validationConditionalTrailerRe.ReplaceAllString(raw, "")
	raw = validationSentenceTrailerRe.ReplaceAllString(raw, "")
	raw = validationTrailerRe.ReplaceAllString(raw, "")
	raw = sentenceTailRe.ReplaceAllString(raw, "")
	return strings.TrimSpace(raw)
}

// ProjectValidationCommand chooses a conservative repository-native validator
// when an engineering task did not name one. Command selection is structural
// controller work, not language-model creativity: the exact result can then be
// handed to the syntax-precise Agent Kernel adapter.
func ProjectValidationCommand(cwd string) string {
	return agentcore.DiscoverRepositoryValidationCommand(cwd)
}

func toolProtocol(profile Profile, cwd string) []string {
	cwdExample := "/workspace"
	if cwd == "" {
		cwdExample = "/current/project"
	}
	shared := []string{
		"Tool-call protocol: for action requests, use the API tool/function-call channel. Do not print JSON, XML, markdown, or a plan as plain text when a tool is required.",
		"Use exact argument names from the tool schema. Common Meadow Code tools: write_file uses file_path and content; read_file/edit use file_path; run_shell_command uses command.",
		"For file operations, pass absolute paths. Resolve relative paths under the current working directory.",
		"If a required detail is missing and cannot be inferred safely, ask one short clarification question instead of guessing.",
	}
	switch profile {
	case ProfileE2B:
		return append(append([]string{
			"E2B profile: be extremely literal and short. For action requests, emit exactly one tool call and no assistant prose before it.",
		}, shared...),
			fmt.Sprintf("Behavioral example, do not print this as text: user asks to create hello.md with Hello -> call API tool write_file with file_path=%s/hello.md and content=Hello followed by a newline.", cwdExample))
	case ProfileE4B:
		return append(append([]string{
			"E4B profile: prefer one precise tool call for simple tasks; use short read/search calls before editing existing files.",
		}, shared...),
			fmt.Sprintf("Behavioral example, do not print this as text: create note.md containing 1+1=? -> call API tool write_file with file_path=%s/note.md and content=1+1=? followed by a newline.", cwdExample),
			fmt.Sprintf(`Behavioral example, do not print this as text: user asks where you are -> answer "%s" in text, no tool needed.`, cwdExample))
	case Profile12B:
		// Dense 12B sits between E4B and 26B: it can follow the full
		// workflow but drifts into prose-instead-of-tool-call more easily
		// than 26B, so the instructions stay literal and example-heavy.
		return append(append([]string{
			"12B profile: follow the coding-agent workflow with concise steps. For every action request you MUST use the API tool/function-call channel — never describe the call in text.",
			"Before modifying existing files, read them first with read_file. For brand-new files, call write_file directly.",
			"One tool call per turn unless the task explicitly needs a sequence.",
		}, shared...),
			fmt.Sprintf("Behavioral example, do not print this as text: create hello-world.md with Hello, world! -> call API tool write_file with file_path=%s/hello-world.md and content=Hello, world! followed by a newline.", cwdExample),
			fmt.Sprintf(`Behavioral example, do not print this as text: user asks where you are -> answer "%s" in text, no tool needed.`, cwdExample))
	case Profile26B:
		return append(append([]string{
			"26B profile: follow the full coding-agent workflow, but still use the tool/function-call channel for actions.",
			"Before modifying existing files, inspect the relevant files first with read_file, grep_search, or glob. For brand-new files, write_file may be used directly.",
		}, shared...),
			fmt.Sprintf("Behavioral example, do not print this as text: create hello-world.md with Hello, world! -> call API tool write_file with file_path=%s/hello-world.md and content=Hello, world! followed by a newline.", cwdExample))
	case ProfileQwen:
		return append(append([]string{
			"Qwen profile: use only the OpenAI function-call/tool-call channel when a tool is required.",
			"Do not print XML, markdown fences, Python function syntax, JSON snippets, or plans as assistant text when a tool is required.",
			"Call exactly one available tool at a time unless the current stage explicitly requires a sequence.",
			"Before modifying existing files, inspect the relevant files first with read_file, grep_search, or glob. For brand-new files, write_file may be used directly.",
		}, shared...),
			fmt.Sprintf("Behavioral example, do not print this as text: create hello-world.md with Hello, world! -> call API tool write_file with file_path=%s/hello-world.md and content=Hello, world! followed by a newline.", cwdExample))
	default:
		return append([]string{
			"Local Gemma profile: use the tool/function-call channel for actions; keep plain text for chat and direct answers.",
		}, shared...)
	}
}

func stageInstruction(plan *Plan, toolNames []string, msgs []provider.Message, cwd string) string {
	if plan == nil {
		return ""
	}
	if plan.ID == IntentEngineeringFix && len(toolNames) == 1 {
		switch toolNames[0] {
		case ToolGrep:
			return "Repository localization stage: search one concise implementation symbol or error keyword. Paths shown in an issue, traceback, or reporter application may not exist in this checkout; do not anchor on them."
		case ToolRead:
			if paths := discoveryPathHints(msgs, 8); len(paths) > 0 {
				return "Repository inspection stage: call read_file for exactly one existing source candidate from the latest discovery result: " +
					strings.Join(paths, ", ") + ". Do not reuse a path that already failed or a path quoted only by the reporter application."
			}
			return "Repository inspection stage: call read_file for exactly one existing path from the latest discovery result. Do not reuse a path that already failed or a path quoted only by the reporter application."
		case ToolGlob:
			return "Repository discovery stage: use a broad source-code glob after the prior semantic search found no matches."
		}
	}
	if plan.ID == IntentResearchArtifact {
		if len(toolNames) == 1 {
			switch toolNames[0] {
			case ToolWebSearch:
				return "Research discovery stage: call web_search with a focused query that can find several useful sources. Do not answer from memory."
			case ToolWebCrawl:
				return "Research crawl stage: call web_crawl on one relevant source site with a narrow query and bounded max_pages."
			case ToolWebFetch, ToolBrowsePage:
				return "Source inspection stage: fetch or browse the exact page needed. Preserve source URLs for the final artifact."
			case ToolDownload:
				return "Image stage: call download_image only with a direct image URL and save under .meadow/research/<artifact_id>/images/. If there is no direct image URL, skip to research_artifact."
			case ToolResearch:
				return "Final artifact stage: call research_artifact now. Put item cards in items_json as a JSON array string. Include source_url for every item, image_path for downloaded images, and sources_json for the source list. Do not print HTML or markdown."
			case ToolAskUser:
				return "Clarification stage: ask one short question only for missing preferences that materially affect the research artifact, such as location, dates, budget, people, style, or size."
			}
		}
		if len(toolNames) > 1 {
			return "Research artifact stage: choose one real API tool call. Ask one short clarification only if essential; otherwise use web_search first. After source-backed findings exist, call research_artifact to create the local page."
		}
		return ""
	}
	if plan.ID != IntentStaticWebArtifact || len(toolNames) != 1 {
		return ""
	}
	switch toolNames[0] {
	case ToolWrite:
		// One-shot：一次寫完整的自包含 .html，HTML 與 JS 同時寫才會內部一致
		//（id 對得上、元素都在）。self-repair 會驗證語法。
		return "用 write_file 一次寫出**完整可玩/可用**的單一 .html 檔，實作用戶要的全部功能：" +
			"所有 CSS 放 <style>、所有 JavaScript 放 <script>，全部內嵌在這一個檔。" +
			"HTML 結構（例如 <canvas> 的 id）必須和你的 JS 對得上。不要只寫骨架、" +
			"不要分成多檔、不要把程式碼貼在對話裡。若用戶指定了檔名就用那個。"
	case ToolShell:
		return "Static web code validation stage: run the named TS/build/typecheck/check command only. Do not inspect visual output here, and do not create or rewrite files with shell commands in this stage."
	case ToolVisual:
		return "Static web visual validation stage: call meadow_browser_visual_check on the generated HTML file. Use Meadow Browser for visual evidence only, not as the code-error path. If the user explicitly requested image or screenshot inspection/comparison, set include_model_image=true so the vision model receives the rendered pixels."
	}
	return ""
}

var discoveryLineRe = regexp.MustCompile(`(?m)^(/workspace/[^:\n]+):[0-9]+:`)

// discoveryPathHints extracts paths that the tools proved exist. Source files
// are shown before tests, docs, examples, and fixtures so a small local model
// doesn't keep following a reporter application's non-repository path after a
// broad search. This only narrows attention; the model still chooses the file.
func discoveryPathHints(msgs []provider.Message, limit int) []string {
	if limit <= 0 {
		return nil
	}
	var preferred, secondary []string
	seen := map[string]bool{}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "tool" {
			continue
		}
		for _, match := range discoveryLineRe.FindAllStringSubmatch(msgs[i].Content, -1) {
			path := match[1]
			if seen[path] {
				continue
			}
			seen[path] = true
			lower := strings.ToLower(path)
			if strings.Contains(lower, "/tests/") || strings.Contains(lower, "/test/") ||
				strings.Contains(lower, "/docs/") || strings.Contains(lower, "/examples/") ||
				strings.Contains(lower, "/fixtures/") || strings.Contains(lower, "/vendor/") ||
				strings.Contains(lower, "/node_modules/") {
				secondary = append(secondary, path)
			} else {
				preferred = append(preferred, path)
			}
		}
		if len(preferred)+len(secondary) > limit*4 {
			break
		}
	}
	paths := append(preferred, secondary...)
	if len(paths) > limit {
		paths = paths[:limit]
	}
	return paths
}

func joinNonEmpty(parts []string, sep string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// actionSystemPrompt ports buildLocalGemma4ActionSystemPrompt.
func actionSystemPrompt(modelID, cwd string, toolNames []string, plan *Plan, userText, stage string) string {
	profile := ProfileForModel(modelID)
	if plan != nil && len(toolNames) == 1 {
		toolName := toolNames[0]
		namedCmd := ""
		if toolName == ToolShell {
			namedCmd = NamedValidationCommand(userText)
		}
		toolSignature := toolName
		switch toolName {
		case ToolWrite:
			toolSignature = "write_file(file_path, content)"
		case ToolRead:
			toolSignature = "read_file(file_path)"
		case ToolGlob:
			toolSignature = "glob(pattern, path)"
		case ToolGrep:
			toolSignature = "grep_search(pattern, path, glob)"
		case ToolShell:
			toolSignature = "run_shell_command(command)"
		case ToolWebSearch:
			toolSignature = "web_search(query)"
		case ToolWebCrawl:
			toolSignature = "web_crawl(url, query, max_pages, max_depth)"
		case ToolWebFetch:
			toolSignature = "web_fetch(url)"
		case ToolBrowsePage:
			toolSignature = "browse_page(url)"
		case ToolVisual:
			toolSignature = "meadow_browser_visual_check(file_path, url, screenshot_path, require_canvas, include_model_image)"
		case ToolInspectImage:
			toolSignature = "inspect_image(file_path, detail)"
		case ToolDownload:
			toolSignature = "download_image(url, file_path)"
		case ToolResearch:
			toolSignature = "research_artifact(title, summary, items_json, sources_json, artifact_id)"
		case ToolAskUser:
			toolSignature = "ask_user_question(question)"
		}
		var toolInstruction []string
		switch toolName {
		case ToolWrite:
			toolInstruction = []string{
				"For this request, call write_file now.",
				"Use an absolute file_path under the current working directory.",
				"Use the requested content or artifact content; add trailing newlines for text files.",
			}
			if plan.ID == IntentOfficeArtifact {
				toolInstruction = append(toolInstruction, "For reports and summaries, preserve source wording for numeric facts and risk phrases; do not paraphrase phrases such as Revenue grew 12%.")
			}
			if plan.ID == IntentSimpleCopy {
				toolInstruction = append(toolInstruction, "For copy tasks, content must exactly match the read_file tool result. Do not summarize or add markdown fences.")
			}
		case ToolRead:
			toolInstruction = []string{
				"For this request, call read_file now for the named input file or one relevant source file from the previous discovery result.",
				"Use an absolute file_path under the current working directory.",
			}
		case ToolGlob:
			toolInstruction = []string{
				"For this request, call glob now to discover real source files.",
				"Use path=/workspace and a broad language-appropriate pattern such as **/*.py, **/*.go, **/*.ts, or **/*.js.",
				"Do not repeat a search expression that already returned zero matches.",
			}
		case ToolGrep:
			toolInstruction = []string{
				"For this request, call grep_search now with a concise symbol or error-keyword pattern.",
				"Use path=/workspace. Do not search for the entire issue sentence or an exact example call.",
			}
		case ToolList:
			toolInstruction = []string{
				"For this request, call list_directory now for the relevant directory.",
				"List before deleting so the destructive confirmation flow can stay narrow.",
			}
		case ToolShell:
			if plan.ID == IntentDeleteGenerated {
				toolInstruction = []string{
					"For this request, call run_shell_command now only after the relevant files have been listed.",
					"Use a narrow deletion command for the exact generated files or folder the user requested.",
					"Do not use broad wildcards, do not delete source files, and do not create or rewrite files in this stage.",
					"Run it from the current working directory.",
				}
			} else if namedCmd != "" {
				toolInstruction = []string{
					fmt.Sprintf("Exact validation command: %s.", namedCmd),
					fmt.Sprintf("For this request, call run_shell_command now with command exactly: %s.", namedCmd),
					"Do not add read, echo, redirection, or other shell text.",
					"Run it from the current working directory.",
				}
			} else {
				toolInstruction = []string{
					"For this request, call run_shell_command now with the validation command named by the user.",
					"Run it from the current working directory.",
				}
			}
		case ToolWebSearch:
			toolInstruction = []string{
				"For this request, call web_search now.",
				"Use a focused query that can discover several useful source URLs.",
				"Do not answer from memory and do not invent links.",
			}
		case ToolWebCrawl:
			toolInstruction = []string{
				"For this request, call web_crawl now on one relevant source site.",
				"Keep max_pages bounded and preserve source URLs for later artifact items.",
			}
		case ToolWebFetch, ToolBrowsePage:
			toolInstruction = []string{
				fmt.Sprintf("For this request, call %s now on the exact source URL needed.", toolName),
				"Preserve source URLs and facts for the final artifact.",
			}
		case ToolVisual:
			toolInstruction = []string{
				"For this request, call meadow_browser_visual_check now on the generated HTML file.",
				"Pass file_path for the artifact that was just written; for Three.js/canvas pages set require_canvas true.",
				"Do not fetch console logs here. Code errors should be handled by syntax validation or the named TS/build/typecheck command.",
				"If the visual check reports a blank or missing canvas, repair with edit/write_file on the next turn.",
			}
		case ToolDownload:
			toolInstruction = []string{
				"For this request, call download_image now only if you have a direct image URL.",
				"Save under .meadow/research/<artifact_id>/images/ with a clear filename.",
				"If you do not have a direct image URL, call research_artifact on the next turn instead of inventing one.",
			}
		case ToolResearch:
			toolInstruction = []string{
				"For this request, call research_artifact now.",
				"items_json must be a JSON array string. Each item should include title, description, source_url, and optionally image_path, image_url, price, reason, tags.",
				"sources_json should be a JSON array string with title, url, excerpt.",
				"Every item must keep its source_url. Do not invent links, prices, images, or source names.",
				"Do not write prose, markdown, or HTML; the tool renders the page.",
			}
		case ToolAskUser:
			toolInstruction = []string{
				"For this request, call ask_user_question now.",
				"Ask one short concrete question only.",
			}
		default:
			toolInstruction = []string{fmt.Sprintf("For this request, call %s now.", toolName)}
		}
		if plan.ID == IntentStaticWebArtifact && toolName == ToolWrite && stage != "" {
			return joinNonEmpty([]string{
				"Call the write_file tool now.",
				strings.Join(plan.Guidance, " "),
				stage,
				"Do not answer in text.",
			}, " ")
		}
		parts := []string{
			"You are Meadow Mamba, a local coding agent.",
			"Available tool: " + toolSignature + ".",
		}
		if cwd != "" {
			parts = append(parts, "Workspace root alias: /workspace. Use /workspace/<relative-path> in file tools.")
		}
		parts = append(parts, plan.Guidance...)
		parts = append(parts, stage)
		parts = append(parts, toolInstruction...)
		parts = append(parts,
			"Use the API tool/function-call channel only.",
			"Do not write prose.",
			"Current local model id: "+orUnknown(modelID)+".",
			"Local tool-call profile: "+string(profile)+".",
			"Prefill intent dictionary result: "+plan.Label+".",
		)
		if len(toolNames) > 0 {
			parts = append(parts, "Available tool names: "+strings.Join(capSlice(toolNames, 24), ", ")+".")
		}
		return joinNonEmpty(parts, " ")
	}

	var context []string
	context = append(context, "Current local model id: "+orUnknown(modelID)+".")
	context = append(context, "Local tool-call profile: "+string(profile)+".")
	if plan != nil {
		context = append(context, "Prefill intent dictionary result: "+plan.Label+".")
	}
	if cwd != "" {
		context = append(context, "Workspace root alias: /workspace. Use /workspace/<relative-path> in file tools.")
	}
	if len(toolNames) > 0 {
		context = append(context, "Available tool names: "+strings.Join(capSlice(toolNames, 24), ", ")+".")
	}
	parts := []string{
		"You are Meadow Mamba, a local coding agent.",
		"The user is asking for an action, not a generic explanation.",
	}
	if plan != nil {
		parts = append(parts, plan.Guidance...)
	}
	parts = append(parts, stage)
	parts = append(parts, toolProtocol(profile, cwd)...)
	parts = append(parts, "After tools finish, summarize the completed action briefly in the user language.")
	parts = append(parts, context...)
	return joinNonEmpty(parts, " ")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func capSlice(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncateText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// --- request shapers (the three rewritten request forms) ---

// MinimalChatMessages ports buildMinimalLocalGemma4ChatMessages: a tiny
// system prompt plus only the latest user message.
func MinimalChatMessages(msgs []provider.Message, modelID, cwd string) []provider.Message {
	latestUser, _ := lastUserMessage(msgs)
	userText := provider.MessageText(latestUser)
	parts := []string{
		"You are Meadow Mamba, a concise local coding assistant.",
		"Answer the user directly in the user language; for Chinese always use Traditional Chinese (zh-TW), never Simplified.",
		"You can read and write files, run shell commands, inspect repositories, and perform coding tasks through Meadow Code tools when the user asks for actions.",
		"For greetings and casual chat, do not mention project files, settings, README files, tools, RAG, or memory unless the user asks.",
		"If the user asks what model you are, identify yourself as Meadow Mamba and include the current local model id.",
		"If the user asks what folder or directory you are in, answer with the current working directory.",
		"Current local model id: " + orUnknown(modelID) + ".",
	}
	if cwd != "" {
		parts = append(parts, "Current working directory: "+cwd+".")
	}
	userMsg := withUserText(latestUser, StripSystemReminder(userText))
	userMsg.Role = "user"
	return []provider.Message{
		{Role: "system", Content: joinNonEmpty(parts, " ")},
		userMsg,
	}
}

// ActionMessages ports buildLocalGemma4ActionMessages: mini action system
// prompt + the right history slice for the stage.
func ActionMessages(msgs []provider.Message, modelID, cwd string, toolNames []string, plan *Plan, anchoredRunText ...string) []provider.Message {
	latestUserText := StripSystemReminder(lastUserText(msgs))
	userText := latestUserText
	if len(anchoredRunText) > 0 && strings.TrimSpace(anchoredRunText[0]) != "" {
		userText = StripSystemReminder(anchoredRunText[0])
		if latestUserText != "" && latestUserText != userText {
			userText += "\n\nLatest controller or user instruction:\n" + latestUserText
		}
	}
	stage := stageInstruction(plan, toolNames, msgs, cwd)
	if errText := lastToolResultText(msgs); strings.HasPrefix(errText, "ERROR:") {
		stage = joinNonEmpty([]string{
			stage,
			"Previous tool result was an error: " + truncateText(errText, 260) + ".",
			"Recover with exactly one corrected real API tool call. Do not print or simulate tool calls, tool results, XML tags, JSON snippets, markdown fences, or a transcript as assistant text.",
			"If an edit failed, re-read the file or use write_file with the complete corrected file content before any validation shell command.",
		}, " ")
	}
	system := provider.Message{
		Role:    "system",
		Content: actionSystemPrompt(modelID, cwd, toolNames, plan, userText, stage),
	}

	hasResult := hasToolResult(msgs)
	keepHistory := IsContextualFollowUp(userText)
	validationOnly := hasResult && len(toolNames) == 1 && toolNames[0] == ToolShell

	switch {
	case validationOnly:
		if plan != nil && plan.ID == IntentEngineeringFix {
			// Keep the successful edit result visible for workflow-state adapter
			// routing. The exact command stays in the action system prompt, while
			// the terminal tool result lets Multi-LoRA select Agent Kernel.
			return append([]provider.Message{system}, compactToolResultHistory(msgs)...)
		}
		return []provider.Message{system, actionUserMessage(msgs, userText)}
	case hasResult:
		// Includes staged static-web writes after the first file: the model
		// must see what it already wrote (live failure: with only
		// [system,user] it stopped after index.html+style.css, never
		// writing script.js — it could not tell anything existed yet).
		return append([]provider.Message{system}, compactToolResultHistory(msgs)...)
	case keepHistory:
		return append([]provider.Message{system}, recentNonSystem(msgs, 8)...)
	default:
		return []provider.Message{system, actionUserMessage(msgs, userText)}
	}
}

func actionUserMessage(msgs []provider.Message, userText string) provider.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		return withUserText(msgs[i], userText)
	}
	return provider.Message{Role: "user", Content: userText}
}

func withUserText(m provider.Message, userText string) provider.Message {
	m.Role = "user"
	m.Content = userText
	if len(m.ContentParts) == 0 {
		return m
	}
	parts := make([]provider.MessageContentPart, 0, len(m.ContentParts)+1)
	replaced := false
	for _, part := range m.ContentParts {
		if part.Type == "text" {
			if replaced {
				continue
			}
			part.Text = userText
			replaced = true
		}
		parts = append(parts, part)
	}
	if !replaced {
		parts = append([]provider.MessageContentPart{{Type: "text", Text: userText}}, parts...)
	}
	m.ContentParts = parts
	return m
}

// FinalMessages ports buildLocalGemma4FinalMessages: tools are done, ask
// for one concise summary.
func FinalMessages(msgs []provider.Message, modelID string, plan *Plan) []provider.Message {
	userText := StripSystemReminder(lastUserText(msgs))
	validation := lastToolResultText(msgs)
	sysParts := []string{
		"You are Meadow Mamba, a local coding agent.",
		"The requested tools have already finished.",
		"Reply with one concise completion summary.",
		"If validation output is provided, include it exactly.",
		"Current local model id: " + orUnknown(modelID) + ".",
	}
	if plan != nil {
		sysParts = append(sysParts, "Prefill intent dictionary result: "+plan.Label+".")
	}
	userParts := []string{"Original request: " + userText}
	if validation != "" {
		userParts = append(userParts, "Validation output:\n"+validation)
	}
	userParts = append(userParts, "Return the final answer now. Do not call tools.")
	return []provider.Message{
		{Role: "system", Content: joinNonEmpty(sysParts, " ")},
		actionUserMessage(msgs, strings.Join(userParts, "\n\n")),
	}
}

// ToolChoice ports buildLocalGemma4ToolChoice: force the single staged tool
// by name, otherwise "required".
func ToolChoice(toolNames []string) any {
	if len(toolNames) == 1 && toolNames[0] != "" {
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": toolNames[0]},
		}
	}
	return "required"
}
