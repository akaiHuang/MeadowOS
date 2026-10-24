// Package router is the Go port of the TS localGemma4IntentRouter plus the
// action-turn machinery around it in provider/mlx.ts. It is deliberately
// deterministic: a small local model needs fewer tool schemas and a clearer
// first step more than it needs another model probe.
package router

import (
	"regexp"
	"strings"
)

// IntentID enumerates the 12 deterministic plans, port of
// LocalGemma4ActionIntentId.
type IntentID string

const (
	IntentSimpleWrite        IntentID = "simple_write"
	IntentSimpleCopy         IntentID = "simple_copy"
	IntentSimpleAppend       IntentID = "simple_append"
	IntentDirectoryCreate    IntentID = "directory_create"
	IntentShellScript        IntentID = "shell_script"
	IntentConfigArtifact     IntentID = "config_artifact"
	IntentDeleteGenerated    IntentID = "delete_generated_files"
	IntentStaticWebArtifact  IntentID = "static_web_artifact"
	IntentEngineeringFix     IntentID = "engineering_fix"
	IntentOfficeArtifact     IntentID = "office_artifact"
	IntentGitAction          IntentID = "git_action"
	IntentInternetResearch   IntentID = "internet_research"
	IntentResearchArtifact   IntentID = "research_artifact"
	IntentInspectOrSearch    IntentID = "inspect_or_search"
	IntentDelegatedLocalTask IntentID = "delegated_local_task"
	IntentGeneralAction      IntentID = "general_action"
)

// Plan is one routed intent: which tools survive and what guidance the
// action prompt carries.
type Plan struct {
	ID       IntentID
	Label    string
	Tools    []string
	Guidance []string
}

// Tool name constants (the model-visible names).
const (
	ToolEdit         = "edit"
	ToolWrite        = "write_file"
	ToolRead         = "read_file"
	ToolInspectImage = "inspect_image"
	ToolGrep         = "grep_search"
	ToolGlob         = "glob"
	ToolShell        = "run_shell_command"
	ToolList         = "list_directory"
	ToolWebFetch     = "web_fetch"
	ToolWebSearch    = "web_search"
	ToolWebCrawl     = "web_crawl"
	ToolBrowsePage   = "browse_page"
	ToolVisual       = "meadow_browser_visual_check"
	ToolInteraction  = "meadow_browser_interaction_check"
	ToolWebMCP       = "meadow_browser_webmcp"
	ToolWebBuild     = "typescript_web_build"
	ToolDownload     = "download_image"
	ToolResearch     = "research_artifact"
	ToolPresentation = "presentation_artifact"
	ToolAskUser      = "ask_user_question"
)

var coreActionTools = []string{ToolRead, ToolWrite, ToolEdit, ToolShell, ToolGrep, ToolGlob, ToolList}
var validatedCreateTools = []string{ToolRead, ToolWrite, ToolShell}

// --- intent predicates, regex-for-regex ports of localGemma4IntentRouter.ts ---

var (
	simpleWriteAskRe             = regexp.MustCompile(`(?i)\b(create|write|make|touch|save)\b|開一個|开一个|建立|創建|创建|新增|寫入|写入|寫.*檔|写.*档|存下來|存下来|存成|存到|存為|存为|儲存|储存|保存|檔案|文件|\.md|\.txt|\.html|\.css|\.js|\.json`)
	simpleWriteVetoRe            = regexp.MustCompile(`(?i)\brun_shell_command\b|\b(read|copy|duplicate|move|rename|delete|remove|append|edit|modify|fix|repair|run|execute|test|build|grep|search|find|list|inspect|npm|node|python|git|shell|terminal)\b|讀|讀取|複製|复制|拷貝|拷贝|搬移|移動|移动|重新命名|改名|刪除|删除|附加|追加|修|修改|改|執行|执行|測試|测试|跑|搜尋|搜索|尋找|查找|列出|檢查|检查`)
	simpleWriteNegatedMutationRe = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|must\s+not|never)\s+(?:edit|modify|change|reformat|add|create|write)\b|(?:不得|不要|不可|禁止|無需|无需|不必)(?:任意|自行)?(?:修改|改動|改动|變更|变更|重排|重新格式化|新增|增加|建立|創建|创建|寫入|写入)`)
	continuationValidationRe     = regexp.MustCompile(`(?i)\b(?:continue|resume|finish)\b.{0,32}\b(?:validation|verification|acceptance|test|check)\b|(?:繼續|继续|接著|接着|恢復|恢复).{0,32}(?:驗收|验收|驗證|验证|測試|测试|檢查|检查|互動|互动)`)
	simpleWriteTransformRe       = regexp.MustCompile(`(?i)\brewrite\s+(?:it\s+)?(?:as|into)\b|改寫成|改写成`)
	simpleWriteReadbackRe        = regexp.MustCompile(`(?i)(?:then\s+)?(?:read|open|inspect)\s+(?:it|the\s+file)?\s*(?:back)?\s*(?:and\s+)?(?:confirm|verify|check)|(?:然後|然后|再)?(?:讀回|读回|讀取|读取|打開|打开).{0,20}(?:確認|确认|檢查|检查|內容|内容)`)

	copyVerbRe    = regexp.MustCompile(`(?i)\b(copy|duplicate)\b|複製|复制|拷貝|拷贝`)
	fileMentionRe = regexp.MustCompile(`[\w./-]+\.[A-Za-z0-9]{1,8}\b`)

	dirCreateRe     = regexp.MustCompile(`(?i)\b(create|make|mkdir|new)\b|建立|創建|创建|新增|開|开`)
	dirIntentRe     = regexp.MustCompile(`(?i)\b(folder|directory|dir)\b|資料夾|文件夾|目录|目錄`)
	dirCreateVetoRe = regexp.MustCompile(`(?i)\b(read|open|view|show|list|ls|delete|remove|rm|move|rename|edit|fix|run|execute|commit)\b|讀|读|查看|打開|打开|顯示|显示|列出|刪|删|移動|移动|重新命名|修改|修|執行|执行|跑|commit`)

	appendIntentRe = regexp.MustCompile(`(?i)\bappend\b|\badd\s+(?:a\s+)?line\b|加一行|加入一行|新增一行|最後.*加|最后.*加|末尾.*加|尾端.*加`)
	appendVetoRe   = regexp.MustCompile(`(?i)\b(create|make|new|delete|remove|rm|move|rename|fix|repair|run|execute|commit)\b|建立|創建|创建|刪|删|移動|移动|重新命名|修|執行|执行|跑|commit`)

	deleteIntentRe      = regexp.MustCompile(`(?i)\b(delete|remove|rm|clean\s+up|clean\s+(?:generated|outputs?|files?|folders?|director(?:y|ies)|workspace|artifacts?)|clear)\b|刪|删|删除|清掉|清理|清空|移除`)
	deleteDescriptionRe = regexp.MustCompile(`(?i)\b(explain|describe|what is|how to|design|principles|backup only)\b|解釋|解释|說明|说明|設計|设计|原則|原则|流程|只.*備份|只.*备份`)
	deleteNegationRe    = regexp.MustCompile(`(?i)\b(?:do not|don't|must not|mustn't|never|without|no)\b|不得|不要|禁止|不可|不能|不准|請勿|请勿|嚴禁|严禁|無須|无需|不必|不應|不应|不該|不该|不希望`)
	deleteFeatureRe     = regexp.MustCompile(`(?i)\b(?:delete|remove|clear)\s+(?:buttons?|items?|entries|records?|cards?|tasks?|expenses?|todos?|rows?|products?|reminders?|board|grid|form|fields?|inputs?)\b|(?:刪除|删除|移除|清空|清除)\s*(?:按鈕|按钮|項目|项目|品項|品项|紀錄|记录|卡片|任務|任务|支出|待辦|待办|留言|提醒|資料列|数据列|列項|列项|棋盤|棋盘|表單|表单|欄位|栏位|輸入|输入|清單|列表|已完成)`)
	resetFeatureRe      = regexp.MustCompile(`(?i)(?:\breset\b|重設|重置).{0,40}(?:\bclear(?:ed)?\b|清空).{0,16}(?:board|grid|cells?|fields?|inputs?|form|list|棋盤|棋盘|格|欄位|栏位|輸入|输入|表單|表单|清單|列表)?`)

	shellScriptRe = regexp.MustCompile(`(?i)\b(shell script|bash script|\.sh\b|bash -n|sh -n|chmod|deploy\.sh|backup script)\b|腳本|脚本`)

	engineeringFixRe = regexp.MustCompile(`(?i)\b(fix|repair|patch|debug|bug|failing|passes|test fails|tests?/|\.test\.|spec\.|pytest|vitest|jest|tsc|ruff|calculator|formatter)\b|修正|修好|修復|修复|除錯|除错|偵錯|调试|測試失敗|测试失败`)

	configArtifactRe = regexp.MustCompile(`(?i)\b(docker-compose|compose\.ya?ml|\.env(?:\.example)?|config|configuration|ya?ml|toml|plist)\b|設定檔|配置`)

	staticWebRe            = regexp.MustCompile(`(?i)\b(static web|web (?:project|app)|index\.html|style\.css|script\.js|html|css|frontend|landing page|website)\b|網頁|网页`)
	threeDWebRe            = regexp.MustCompile(`(?i)\b(three\.?js|webgl|3d|orbitcontrols|low[-\s]?poly)\b|三維|立體|拖曳視角|拖拽视角`)
	staticWebCreateRe      = regexp.MustCompile(`(?i)\b(create|make|build|develop|write|generate|produce|implement|code)\b|做|製作|制作|建立|創建|创建|開發|开发|生成|產生|产生|實作|实现|寫|写`)
	officeArtifactCreateRe = regexp.MustCompile(`(?i)\b(create|make|build|generate|produce|deliver|write|save|complete)\b|建立|創建|创建|製作|制作|生成|產生|产生|完成|交付|寫入|写入|存成|存到|存為|存为|儲存|储存|保存`)

	officeArtifactRe     = regexp.MustCompile(`(?i)\b(csv|json|xlsx|docx|pptx|pdf|report|brief|summary|leadership|executive|workbook|deck|slides?|accounts?|pipeline|forecast|customer(?:_|\s+)notes|customer(?:_|\s+)call|follow[-\s]?up|email|action(?:_|\s+)plan|meeting(?:_|\s+)notes|calendar|ics|schedule|next steps?)\b|報告|报告|摘要|簡報|简报|表格|試算表|试算表|會議|会议|行動方案|行动方案|跟進|跟进|郵件|邮件|行事曆|日曆|日历`)
	presentationCreateRe = regexp.MustCompile(`(?i)(?:\b(?:create|make|build|generate|produce|deliver)\b.{0,64}\b(?:presentation|deck|slides?|pptx)\b|(?:建立|製作|制作|生成|產生|产生|完成|交付).{0,40}(?:簡報|简报|投影片))`)
	presentationToolRe   = regexp.MustCompile(`(?i)\bpresentation_artifact\b`)
	// Negation must apply to creating/using the deliverable itself. Broadly
	// scanning from "不要" to the next "簡報" misclassifies constraints such as
	// "不要手寫簡報框架" as "do not create a presentation", disabling the very
	// completion gate the user explicitly requested.
	presentationNegationRe = regexp.MustCompile(`(?i)(?:\b(?:do not|don't|must not)\b\s+(?:(?:create|make|build|generate|produce|deliver|use|call)\s+(?:a\s+)?)?(?:presentation_artifact|presentation|deck|slides?)\b|\bwithout\s+(?:a\s+)?(?:presentation_artifact|presentation|deck|slides?)\b|(?:不得|不要|禁止|無需|无需|不必)\s*(?:(?:建立|製作|制作|生成|產生|产生|交付|使用|呼叫|调用)\s*)?(?:presentation_artifact|簡報|简报|投影片))`)

	// Bare "status" and "diff" are common filename stems. Requiring either an
	// explicit Git term or an action phrase prevents status.txt from exposing
	// the repository tool surface to an unrelated file-creation request.
	gitActionRe = regexp.MustCompile(`(?i)\b(git|commit|branch|pull|push|merge)\b|\b(?:show|check|view)\s+(?:the\s+)?(?:status|diff)(?:\s|$|[?!,;:])`)

	internetResearchRe  = regexp.MustCompile(`(?i)https?://|\b(web search|internet|online|latest|recent|weather|search web|look up|crawl|scrape|fetch url|research .*website|research .*webpage|docs site|meadow browser)\b|網路|网络|最新|天氣|天气|查資料|找資料|研究.*網站|研究.*网页|爬蟲|爬虫|爬取`)
	externalKnowledgeRe = regexp.MustCompile(`(?i)\b(20(2[5-9]|[3-9][0-9])|up[- ]?to[- ]?date|as of|today|this year|this month|recent|latest|news|price|pricing|stock|exchange rate|benchmark|score|ranking|schedule|release|released|version|model card|official docs|law|regulation|policy|current\s+(president|prime minister|ceo|governor|law|price|version)|who\s+is\s+the\s+current)\b|(?:qwen|gemma|llama|mistral|deepseek|gpt|claude)[\w. -]{0,80}(?:capabilit|benchmark|release|version|support)|(?:qwen|gemma|llama|mistral|deepseek|gpt|claude|模型).{0,80}(?:能力|跑分|評測|版本|發布|支援)|現任|今天|今日|今年|本月|最近|最新|新聞|價格|價錢|多少錢|股價|股票|匯率|天氣|氣象|颱風|台風|地震|跑分|分數|評測|評價|排名|賽程|比分|發布|釋出|上市|模型卡|官方文件|法規|法律|政策|目前.*(總統|首相|總理|CEO|執行長|價格|價錢|版本|新聞|法規|法律|政策|狀態|支援|支援度)|比較.*(benchmark|跑分|分數|評測)|20(2[5-9]|[3-9][0-9])年`)
	explicitNoNetworkRe = regexp.MustCompile(`(?i)\b(?:do not|don't|must not|without|no need to)\s+(?:use\s+)?(?:the\s+)?(?:external\s+)?(?:web|internet|online|network|web search|search(?:\s+the)?\s+web)\b|(?:不得|不要|禁止|無需|无需|不必|不可|不能)(?:使用|進行|进行)?(?:外部)?(?:上網|上网|搜尋網路|搜索网络|網路搜尋|网络搜索|網路|网络)`)

	researchArtifactRe = regexp.MustCompile(`(?i)\b(outfit|clothing|wardrobe|lookbook|itinerary|travel plan|trip plan|shopping|zara|h&m|uniqlo|product links?|compare products?|research page|research artifact)\b|穿搭|服裝|服装|衣服|褲子|裤子|外套|(?:^|[^執执])行程|旅行|旅遊|旅游|東京|东京|大阪|商品|比價|比价|購物|购物|整理成.{0,8}網頁|整理成.{0,8}网页|做成.{0,8}網頁|做成.{0,8}网页|產出.{0,8}頁面|产出.{0,8}页面|生成.{0,8}頁面|生成.{0,8}页面`)

	inspectSearchRe = regexp.MustCompile(`(?i)\b(read|inspect|search|find|grep|list|show|open|where|pwd|ls)\b|讀|讀取|搜尋|搜索|尋找|查找|列出|檢查|检查|在哪|哪裡|哪里`)

	delegatedLocalContractRe = regexp.MustCompile(`(?i)(?:\b(?:read|open|inspect|load|review)\b|閱讀|阅读|讀取|读取|查看|載入|载入)[^.!?\n。！？]{0,64}[\w./-]+\.(?:md|txt|rst|adoc|json|ya?ml|toml)\b`)
	delegatedLocalWorkRe     = regexp.MustCompile(`(?i)\b(follow|complete|execute|implement|finish|modify|edit|deliver)\b|依照|按照|完成|執行|执行|實作|实现|修改|交付|只修改`)
	delegatedExplicitWebRe   = regexp.MustCompile(`(?i)https?://|\b(search\s+(?:the\s+)?web|web\s+search|internet|online|latest|recent|weather|news|price|stock|exchange rate|crawl|scrape|website|webpage)\b|上網|上网|網路|网络|搜尋網路|搜索网络|網站|网站|網頁|网页|最新|最近|天氣|天气|新聞|新闻|價格|价格|股價|股价|匯率|汇率|爬蟲|爬虫|爬取`)
)

// isDelegatedLocalTask recognizes a generic task-file workflow: the user asks
// the agent to read a local instruction/spec file, then carry out the work and
// modify permitted deliverables. This must outrank broad words such as
// "benchmark" or "評測", which may describe the workspace rather than request
// current web data. It deliberately keys on workflow shape, not a particular
// filename, benchmark suite, or expected answer.
func isDelegatedLocalTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	return delegatedLocalContractRe.MatchString(t) &&
		delegatedLocalWorkRe.MatchString(t) &&
		(!delegatedExplicitWebRe.MatchString(t) || explicitNoNetworkRe.MatchString(t))
}

// TaskRequiresPresentationArtifact recognizes an explicit typed-presentation
// deliverable. It is a completion capability contract, not a filename guess:
// presentation_artifact owns its output directory and the user need not name
// index.html or a generated source file. Negated discussion of a presentation
// deliberately does not become an execution obligation.
func TaskRequiresPresentationArtifact(text string) bool {
	t := StripSystemReminder(text)
	if strings.TrimSpace(t) == "" || presentationNegationRe.MatchString(t) {
		return false
	}
	return presentationToolRe.MatchString(t) || presentationCreateRe.MatchString(t)
}

func isSimpleWriteTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len([]rune(t)) > 260 {
		return false
	}
	if threeDWebRe.MatchString(t) {
		return false
	}
	// A follow-up such as "continue the previous interaction acceptance; do
	// not add features" is a validation continuation. Creation words inside a
	// negated scope must never collapse that task to a one-tool write route.
	if continuationValidationRe.MatchString(t) {
		return false
	}
	// Negative output constraints such as "不得改動數字" describe literal
	// fidelity; they are not a request to edit an existing artifact. Remove only
	// that bounded negated phrase before applying the mutation/read veto.
	vetoText := simpleWriteNegatedMutationRe.ReplaceAllString(t, "")
	vetoText = simpleWriteTransformRe.ReplaceAllString(vetoText, "")
	return simpleWriteAskRe.MatchString(t) && !simpleWriteVetoRe.MatchString(vetoText)
}

func isSimpleWriteReadbackTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len([]rune(t)) > 320 || staticWebRe.MatchString(t) || threeDWebRe.MatchString(t) {
		return false
	}
	if !simpleWriteAskRe.MatchString(t) || !simpleWriteReadbackRe.MatchString(t) || !fileMentionRe.MatchString(t) {
		return false
	}
	// Remove only the bounded read-back obligation. Any other mutation,
	// execution, research, or inspection verb still vetoes this compact route.
	vetoText := simpleWriteReadbackRe.ReplaceAllString(t, "")
	vetoText = simpleWriteNegatedMutationRe.ReplaceAllString(vetoText, "")
	vetoText = simpleWriteTransformRe.ReplaceAllString(vetoText, "")
	return !simpleWriteVetoRe.MatchString(vetoText)
}

func isSimpleCopyTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len([]rune(t)) > 320 {
		return false
	}
	if !copyVerbRe.MatchString(t) {
		return false
	}
	return len(fileMentionRe.FindAllString(t, -1)) >= 2
}

func isDirectoryCreateTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len([]rune(t)) > 260 {
		return false
	}
	if !dirCreateRe.MatchString(t) || !dirIntentRe.MatchString(t) {
		return false
	}
	return !dirCreateVetoRe.MatchString(t)
}

func isSimpleAppendTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len([]rune(t)) > 360 {
		return false
	}
	if !appendIntentRe.MatchString(t) {
		return false
	}
	if len(fileMentionRe.FindAllString(t, -1)) != 1 {
		return false
	}
	return !appendVetoRe.MatchString(t)
}

func isDeleteGeneratedFilesTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len([]rune(t)) > 360 {
		return false
	}
	if deleteDescriptionRe.MatchString(t) {
		return false
	}
	if isStaticWebArtifactTask(t) && staticWebCreateRe.MatchString(t) &&
		(deleteFeatureRe.MatchString(t) || resetFeatureRe.MatchString(t)) {
		// A delete/remove control inside a requested web product is application
		// behaviour, not authorization to delete host files. Treating CRUD copy
		// such as "包含刪除項目" as destructive blocked ordinary product work at
		// admission before the model or the workspace sandbox was involved.
		return false
	}
	for _, match := range deleteIntentRe.FindAllStringIndex(t, -1) {
		prefix := t[:match[0]]
		clauseStart := 0
		for _, boundary := range []string{
			"\n", ".", "!", "?", ";", ",", "。", "！", "？", "；", "，",
			" but ", " however ", "但是", "不過", "不过", "但", "卻", "却",
		} {
			if index := strings.LastIndex(prefix, boundary); index >= 0 {
				candidate := index + len(boundary)
				if candidate > clauseStart {
					clauseStart = candidate
				}
			}
		}
		clausePrefix := prefix[clauseStart:]
		if deleteNegationRe.MatchString(clausePrefix) ||
			strings.HasSuffix(strings.TrimSpace(clausePrefix), "不") {
			continue
		}
		return true
	}
	return false
}

func isStaticWebArtifactTask(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if !(staticWebRe.MatchString(t) || threeDWebRe.MatchString(t)) {
		return false
	}
	if officeArtifactRe.MatchString(t) && NeedsComputation(t) {
		return false
	}
	if internetResearchRe.MatchString(t) &&
		!staticWebValidationTask(t) &&
		!threeDWebRe.MatchString(t) &&
		TaskTargetFile(t) == "" &&
		!staticWebCreateRe.MatchString(t) {
		return false
	}
	return true
}

func staticWebCreationPrecedesRepair(text string) bool {
	create := staticWebCreateRe.FindStringIndex(text)
	if create == nil {
		return false
	}
	repair := engineeringFixRe.FindStringIndex(text)
	return repair == nil || create[0] < repair[0]
}

// officeArtifactCreationPrecedesRepair distinguishes the primary deliverable
// from validation language later in the same request.  A data task such as
// "read inputs, create result.json; if the checker conflicts, fix the checker"
// is not an existing code repair merely because its acceptance clause contains
// the word "fix".  Conversely, "fix the report generator, then create a report"
// remains an engineering repair because the repair instruction came first.
func officeArtifactCreationPrecedesRepair(text string) bool {
	if !officeArtifactRe.MatchString(text) || len(TaskTargetFiles(text)) == 0 {
		return false
	}
	create := officeArtifactCreateRe.FindStringIndex(text)
	if create == nil {
		return false
	}
	repair := engineeringFixRe.FindStringIndex(text)
	return repair == nil || create[0] < repair[0]
}

// staticWebValidationTask keeps an explicit build/visual verification of an
// existing HTML/TypeScript artifact on the static-web pipeline even though no
// create verb is present. Without this, the filename alone can fall through to
// simple_write and its mutation-only finalizer.
func staticWebValidationTask(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if !strings.Contains(t, ".html") {
		return false
	}
	hasSource := strings.Contains(t, ".ts") || strings.Contains(t, "typescript")
	hasValidation := strings.Contains(t, "build") || strings.Contains(t, "compile") ||
		strings.Contains(t, "validate") || strings.Contains(t, "verify") ||
		strings.Contains(t, "編譯") || strings.Contains(t, "编译") ||
		strings.Contains(t, "驗證") || strings.Contains(t, "验证") ||
		strings.Contains(t, "視覺驗證") || strings.Contains(t, "视觉验证") ||
		strings.Contains(t, "meadow browser")
	return hasSource && hasValidation
}

// HasExplicitWebResearchCue reports whether the user asked for current or
// website-backed information. Route-predictor hints use this as a safety guard
// so a vague local artifact request cannot be promoted into network access.
func HasExplicitWebResearchCue(userText string) bool {
	t := strings.TrimSpace(userText)
	if t == "" {
		return false
	}
	return internetResearchRe.MatchString(t) || externalKnowledgeRe.MatchString(t)
}

func staticWebArtifactPlan(text string) *Plan {
	guidance := []string{
		"Prefill intent: create or modify a small web project.",
		"Action task tool strategy: Static web artifact.",
		"First step: write the requested HTML/CSS/JS files, or inspect existing files before editing.",
		"Validation step: run a named TS/build check when provided, then use Meadow Browser for deterministic DOM, canvas, console, runtime, and network diagnostics.",
		"Screenshots are human-review evidence by default. If the user explicitly asks to inspect or compare an image or screenshot, set meadow_browser_visual_check.include_model_image=true and reason from the attached pixels; a still image cannot prove motion or interaction.",
		"If validation or the user reports an error: fix and re-validate, at most twice; after two failed fixes stop and report what still fails.",
	}
	if threeDWebRe.MatchString(text) {
		guidance = append(guidance,
			"Three.js/3D artifact requirements: load Three.js from a CDN, add camera.lookAt(0, 0, 0) or OrbitControls, and make the first viewport visibly show the scene, not only sky/background.",
			"For grassland-style scenes, create many visible grass blades or clumps, several low-poly trees with trunks/crowns, a clear path, lighting, and a camera angle similar to an explorable 3D world.",
		)
	}
	return &Plan{
		ID: IntentStaticWebArtifact, Label: "static web artifact",
		// edit included for incremental build: skeleton (write) → fill
		// the script placeholder (edit), each a focused small generation.
		Tools:    []string{ToolWrite, ToolEdit, ToolRead, ToolShell, ToolVisual, ToolList},
		Guidance: guidance,
	}
}

// Classify ports classifyLocalGemma4ActionIntent — same predicate order,
// general_action as the always-matching fallback.
func Classify(userText string) *Plan {
	t := strings.TrimSpace(userText)
	if t == "" {
		return nil
	}
	if IsArtifactDiscussionQuestion(t) {
		return &Plan{
			ID: IntentGeneralAction, Label: "artifact discussion",
			Guidance: []string{
				"Answer the user's conceptual or hypothetical question directly.",
				"Do not create, inspect, or modify files unless the user explicitly asks you to perform that action.",
			},
		}
	}

	if isSimpleCopyTask(t) {
		return &Plan{
			ID: IntentSimpleCopy, Label: "simple file copy",
			Tools: []string{ToolRead, ToolWrite},
			Guidance: []string{
				"Prefill intent: copy one local file to another local file.",
				"First step: call read_file for the source file.",
				"Second step: call write_file for the destination file with exactly the content returned by read_file.",
				"Do not answer in prose before the required tool call.",
			},
		}
	}
	if isDirectoryCreateTask(t) {
		return &Plan{
			ID: IntentDirectoryCreate, Label: "directory creation",
			Tools: []string{ToolShell},
			Guidance: []string{
				"Prefill intent: create a local directory.",
				"First step: call run_shell_command with mkdir -p for the requested directory under the current working directory.",
				"Use a relative path argument in the shell command when the user gave a relative directory path.",
				"Do not answer in prose before the required tool call.",
			},
		}
	}
	if isSimpleAppendTask(t) {
		return &Plan{
			ID: IntentSimpleAppend, Label: "simple file append",
			Tools: []string{ToolShell},
			Guidance: []string{
				"Prefill intent: append one line to an existing local file.",
				"First step: call run_shell_command to append exactly the requested line to the requested file.",
				"Use a relative path argument in the shell command when the user gave a relative file path.",
				"Do not inspect unrelated project files or answer in prose before the required tool call.",
			},
		}
	}
	if isDeleteGeneratedFilesTask(t) {
		return &Plan{
			ID: IntentDeleteGenerated, Label: "delete generated files",
			Tools: []string{ToolList, ToolShell},
			Guidance: []string{
				"Prefill intent: clean up or delete generated local files.",
				"First step: list the relevant directory or generated files before deleting.",
				"Deletion must use the existing destructive confirmation flow; never broaden the target beyond files the user named or files Meadow generated in this run.",
				"Do not create, rewrite, or explain files before the required cleanup tool call.",
			},
		}
	}
	// A concrete web creation remains a web artifact even when the user also
	// says "save the file". The generic simple-write predicate intentionally
	// recognizes those words, but routing a playable HTML task through that
	// one-tool plan removes browser validation and caused prose-only/unfinished
	// deliveries in the product UI.
	if isStaticWebArtifactTask(t) && (staticWebCreationPrecedesRepair(t) || staticWebValidationTask(t)) {
		return staticWebArtifactPlan(t)
	}
	if isSimpleWriteReadbackTask(t) {
		return &Plan{
			ID: IntentSimpleWrite, Label: "simple file creation with read-back",
			Tools: []string{ToolWrite, ToolRead},
			Guidance: []string{
				"Prefill intent: create one new file, then read back that exact path.",
				"First call write_file with the requested literal content; after it succeeds, call read_file on the confirmed path and compare the returned bytes before completing.",
				"Do not inspect unrelated files or use repository, shell, web, or office tools.",
			},
		}
	}
	if isSimpleWriteTask(t) {
		return &Plan{
			ID: IntentSimpleWrite, Label: "simple file creation",
			Tools: []string{ToolWrite},
			Guidance: []string{
				"Prefill intent: create a new file directly.",
				"First step: call write_file with an absolute file_path and the requested content.",
				"Do not answer in prose before the write_file call.",
			},
		}
	}
	if shellScriptRe.MatchString(t) && !TaskRequiresPresentationArtifact(t) {
		return &Plan{
			ID: IntentShellScript, Label: "shell script with validation",
			Tools: []string{ToolWrite, ToolShell, ToolRead},
			Guidance: []string{
				"Prefill intent: create or repair a shell script.",
				"Action task tool strategy: Shell script with Explicit validation.",
				"First step: use write_file or read_file according to whether the script already exists.",
				"Validation step: run the named shell command, or bash -n/sh -n for syntax when no command is named.",
			},
		}
	}
	// A create-new web task can still name a checker and say "validation
	// fails". Those repair words describe the requested post-write loop, not
	// an existing engineering bug. Route the explicit artifact creation before
	// engineering_fix so turn one exposes write_file instead of discovery-only
	// tools. Existing-page bug fixes (no create verb) continue to use the
	// engineering repair plan below.
	if isDelegatedLocalTask(t) && !TaskRequiresPresentationArtifact(t) {
		return &Plan{
			ID:    IntentDelegatedLocalTask,
			Label: "delegated local task",
			Tools: append([]string{}, coreActionTools...),
			Guidance: []string{
				"Prefill intent: execute work described by a local instruction or specification file.",
				"First step: read the named local instruction/specification file before choosing how to perform the task.",
				"After reading it, choose the necessary local file or shell tools yourself; do not use web tools unless the user explicitly requested web research.",
				"For long programs, use write_file to create a script first, then run it with run_shell_command; do not embed a large program in a shell one-liner or heredoc.",
				"Only modify deliverables permitted by the instruction file, then validate the result with an appropriate local command.",
			},
		}
	}
	if engineeringFixRe.MatchString(t) && !officeArtifactCreationPrecedesRepair(t) {
		return &Plan{
			ID: IntentEngineeringFix, Label: "engineering repair",
			Tools: []string{ToolList, ToolGlob, ToolGrep, ToolRead, ToolEdit, ToolWrite, ToolShell},
			Guidance: []string{
				"Prefill intent: inspect code, patch the bug, then validate.",
				"Action task tool strategy: Explicit validation for an engineering fix.",
				"First step: read the named source/test file. If the user did not name an exact file path, first use list_directory, glob, or grep_search to discover real files; never guess src/index.js or a test filename.",
				"Validation step: run the exact test command named by the user after editing.",
				"If validation still fails: fix and re-validate, at most twice; after two failed fixes stop and report what still fails.",
			},
		}
	}
	if configArtifactRe.MatchString(t) {
		return &Plan{
			ID: IntentConfigArtifact, Label: "configuration artifact",
			Tools: append([]string{}, validatedCreateTools...),
			Guidance: []string{
				"Prefill intent: create or update configuration files.",
				"Action task tool strategy: Config artifact.",
				"First step: read existing config when present; otherwise write the requested config files.",
				"Validation step: run the named check or use a parser/lint command when available.",
			},
		}
	}
	if researchArtifactRe.MatchString(t) {
		return &Plan{
			ID: IntentResearchArtifact, Label: "browser research artifact",
			Tools: []string{
				ToolAskUser,
				ToolWebSearch,
				ToolWebCrawl,
				ToolWebFetch,
				ToolBrowsePage,
				ToolDownload,
				ToolResearch,
				ToolRead,
				ToolEdit,
			},
			Guidance: []string{
				"Prefill intent: collect several source-backed findings, optional images, then create a local interactive research artifact page.",
				"For shopping, outfit, travel, itinerary, and comparison tasks: use multiple source URLs when possible; never invent links.",
				"If required user preferences are missing (location, dates, budget, people, style, size), ask one short question with ask_user_question before researching.",
				"Use web_search for broad discovery, web_crawl for one site with several related pages, web_fetch or browse_page for exact source pages, download_image for direct image URLs, then research_artifact for the final page.",
				"Every final item should include a source_url. Use image_path when download_image succeeded, or image_url only when it is a direct image URL.",
			},
		}
	}
	// Local HTML/Three.js artifact prompts often include CDN URLs. Those URLs
	// are dependencies, not a request to research the web, so this must stay
	// before internet_research.
	if isStaticWebArtifactTask(t) {
		return staticWebArtifactPlan(t)
	}
	if !explicitNoNetworkRe.MatchString(t) &&
		(internetResearchRe.MatchString(t) ||
			(externalKnowledgeRe.MatchString(t) && !officeArtifactCreationPrecedesRepair(t))) {
		return &Plan{
			ID: IntentInternetResearch, Label: "internet research",
			Tools: []string{ToolWebCrawl, ToolWebSearch, ToolWebFetch, ToolBrowsePage},
			Guidance: []string{
				"Prefill intent: collect current or website-backed information from the web.",
				"Start with tool calls immediately. Privately form a short working hypothesis, but do not stream an unsupported preamble before searching.",
				"For a capability or release question, use at most two complementary searches (one official/source-owner query and one independent evaluation query), then open only the most relevant distinct sources.",
				"If the answer depends on facts that may have changed, do not answer from model memory.",
				"Use web_crawl for one site with several related pages, web_search for broad discovery, web_fetch for one known URL, and browse_page for JS-heavy pages.",
				"Return source URLs in the final answer.",
			},
		}
	}
	// A read→compute→report task ("read sales.csv, compute totals, make
	// report.html") matches BOTH staticWebRe (html) and officeArtifactRe
	// (report/csv). It is not a hand-authored page: the model must read the
	// input and compute, not write a self-contained page from scratch. Let the
	// office_artifact block below claim it (it reads inputs first and offers
	// run_shell_command for the computation). Without this guard it misroutes to
	// static_web, gets the snake example, never reads the CSV, and computes in
	// its head — the root cause of the accounting failures.
	if (staticWebRe.MatchString(t) || threeDWebRe.MatchString(t)) && !(officeArtifactRe.MatchString(t) && NeedsComputation(t)) {
		guidance := []string{
			"Prefill intent: create or modify a small web project.",
			"Action task tool strategy: Static web artifact.",
			"First step: write the requested HTML/CSS/JS files, or inspect existing files before editing.",
			"Validation step: run a named TS/build check when provided, then use Meadow Browser for deterministic DOM, canvas, console, runtime, and network diagnostics.",
			"Screenshots are human-review evidence by default. If the user explicitly asks to inspect or compare an image or screenshot, set meadow_browser_visual_check.include_model_image=true and reason from the attached pixels; a still image cannot prove motion or interaction.",
			"If validation or the user reports an error: fix and re-validate, at most twice; after two failed fixes stop and report what still fails.",
		}
		if threeDWebRe.MatchString(t) {
			guidance = append(guidance,
				"Three.js/3D artifact requirements: load Three.js from a CDN, add camera.lookAt(0, 0, 0) or OrbitControls, and make the first viewport visibly show the scene, not only sky/background.",
				"For grassland-style scenes, create many visible grass blades or clumps, several low-poly trees with trunks/crowns, a clear path, lighting, and a camera angle similar to an explorable 3D world.",
			)
		}
		return &Plan{
			ID: IntentStaticWebArtifact, Label: "static web artifact",
			// edit included for incremental build: skeleton (write) → fill
			// the script placeholder (edit), each a focused small generation.
			Tools:    []string{ToolWrite, ToolEdit, ToolRead, ToolShell, ToolVisual, ToolList},
			Guidance: guidance,
		}
	}
	if officeArtifactRe.MatchString(t) {
		guidance := []string{
			"Prefill intent: read source material and create office/data artifacts.",
			"Action task tool strategy: Data artifact and Document artifact.",
			"First step: read the named inputs before writing outputs.",
			"Validation step: run the named checker or verify required output files exist and contain requested sections/columns.",
		}
		if NeedsComputation(t) {
			// Standing compute discipline (replaces the old separately-gated,
			// turn-1-only engine injection that the staged tool-pin made
			// unfollowable). Present every turn this plan is active.
			guidance = append(guidance,
				"This task needs computed numbers (totals/sums/averages): do NOT do mental arithmetic — a weak model gets them wrong. "+
					"Write a short Python script to a file (e.g. calc.py) with write_file (do NOT use python3 -c with a multi-line program — nested quotes break under the shell), "+
					"run it with run_shell_command (python3 calc.py), then write the computed numbers into the requested output file (not only an intermediate data file).")
		}
		return &Plan{
			ID: IntentOfficeArtifact, Label: "office/data artifact",
			Tools:    append([]string{}, validatedCreateTools...),
			Guidance: guidance,
		}
	}
	if gitActionRe.MatchString(t) {
		return &Plan{
			ID: IntentGitAction, Label: "git/repository action",
			Tools: []string{ToolShell, ToolRead, ToolGrep, ToolGlob},
			Guidance: []string{
				"Prefill intent: inspect or operate on git/repository state.",
				"First step: use run_shell_command for git/status commands, then inspect files only if needed.",
			},
		}
	}
	if inspectSearchRe.MatchString(t) {
		return &Plan{
			ID: IntentInspectOrSearch, Label: "inspect/search",
			Tools: []string{ToolRead, ToolGrep, ToolGlob, ToolList, ToolShell},
			Guidance: []string{
				"Prefill intent: inspect project state before answering.",
				"First step: use the narrowest read/search/list tool that answers the user request.",
			},
		}
	}
	return &Plan{
		ID: IntentGeneralAction, Label: "general local action",
		Tools: append([]string{}, coreActionTools...),
		Guidance: []string{
			"Prefill intent: local action request.",
			"First step: choose the narrowest file or shell tool needed to make progress.",
			"Validation step: run a named validator when the user provided one.",
		},
	}
}

// SelectIntentTools ports selectLocalGemma4IntentToolNames: classify, then
// intersect the plan's tools with what is actually registered. Returns nil
// when nothing survives.
func SelectIntentTools(userText string, availableToolNames []string) (*Plan, []string) {
	plan := Classify(userText)
	if plan == nil {
		return nil, nil
	}
	available := map[string]bool{}
	for _, n := range availableToolNames {
		available[n] = true
	}
	var selected []string
	for _, n := range plan.Tools {
		if available[n] {
			selected = append(selected, n)
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	return plan, selected
}
