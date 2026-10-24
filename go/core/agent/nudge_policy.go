package agent

import (
	"fmt"
	"strings"
)

// NudgeKind names a generic recovery signal. It describes why the controller
// asks the model to continue; it must not contain task names, filenames, or
// expected answers.
type NudgeKind string

const (
	NudgeSyntaxRepair      NudgeKind = "syntax_repair"
	NudgeToolError         NudgeKind = "tool_error"
	NudgeValidationFailure NudgeKind = "validation_failure"
	NudgeStagedDeliverable NudgeKind = "staged_deliverable"
	NudgeNamedDeliverable  NudgeKind = "named_deliverable"
	NudgeForcedTool        NudgeKind = "forced_tool"
	NudgeRunIntent         NudgeKind = "run_intent"
	NudgeTaskStart         NudgeKind = "task_start"
	NudgeTaskContinue      NudgeKind = "task_continue"
	NudgeStrategyChange    NudgeKind = "strategy_change"
	NudgeEvidenceRequired  NudgeKind = "evidence_required"
	NudgeEvidenceStatus    NudgeKind = "evidence_status"
	NudgeRequirementAudit  NudgeKind = "requirement_audit"
	NudgeAuditEvidence     NudgeKind = "audit_evidence"
	NudgePlanActive        NudgeKind = "plan_active"
	NudgeOutputTruncated   NudgeKind = "output_truncated"
	NudgeRepeatedTool      NudgeKind = "repeated_tool"
)

const sourceBackedAuditNudge = " 若產物是由權威輸入生成的報告或表格，必須比對產物中每個實際寫出的資料列與具體事實，不可只驗結構化 answer 或模型自己的 validator。"

type NudgeDecision struct {
	Kind    NudgeKind
	Allowed bool
	Attempt int
	Limit   int
}

// NudgeInput is the structured data needed to compose a recovery prompt. It
// carries runtime facts, never an expected task answer.
type NudgeInput struct {
	Kind              NudgeKind
	Target            string
	Targets           []string
	ToolName          string
	Task              string
	Failure           string
	ValidationCommand string
	NeedsComputation  bool
	SyntaxFile        string
	SyntaxError       string
	Verification      VerificationStatus
	ChangedPaths      []string
	ActivePlanItems   int
	PreserveHistory   bool
}

func ComposeNudge(input NudgeInput) string {
	switch input.Kind {
	case NudgeSyntaxRepair:
		return fmt.Sprintf(
			"(%s 仍有語法錯誤：%s。立即用 edit 工具修正這個錯誤——只改錯的地方、不要重寫整個檔案。修好程式才能跑。)",
			input.SyntaxFile, input.SyntaxError)
	case NudgeToolError:
		command := ""
		if input.ValidationCommand != "" {
			command = " 驗證時必須呼叫 run_shell_command，command exactly: " + input.ValidationCommand + "。"
		}
		return "(上一個工具失敗，任務尚未完成：" + input.Failure + "。不要用文字總結或宣稱完成；" +
			"請用 read_file / edit / write_file 修正問題，修好後再執行必要驗證。" + command + ")"
	case NudgeValidationFailure:
		command := ""
		if input.ValidationCommand != "" {
			command = " 最後必須再次呼叫 run_shell_command，command exactly: " + input.ValidationCommand + "，且通過後才能總結。"
		}
		return "(前一次驗證命令失敗，任務尚未完成。用 read_file / edit / write_file 修正失敗原因；" +
			"不要宣稱測試通過、不要只用文字說明。" + command + ")"
	case NudgeStagedDeliverable:
		return fmt.Sprintf(
			"(檔案尚未寫入磁碟（缺 %s）。立即用 write_file 工具把內容寫入檔案；不要把程式碼貼在對話中。)",
			strings.Join(input.Targets, ", "))
	case NudgeNamedDeliverable:
		if input.NeedsComputation {
			return "(你還沒產出 " + input.Target + "，而且還沒實際算過數字。先用 run_shell_command 跑一段 Python 算出正確結果，" +
				"再用 write_file 把那些數字寫進 " + input.Target + "——不要用心算、也不要只把內容貼在對話裡。)"
		}
		return "(你還沒產出任務要求的檔案 " + input.Target + "。用 write_file 把它寫出來——" +
			"若前面已用工具算出數字或讀到資料，把那些結果寫進這個檔案裡。)"
	case NudgeForcedTool:
		return fmt.Sprintf(
			"(上一輪需要呼叫 %s 工具，但你只輸出文字、沒有工具呼叫。原始需求：%s。現在不要回答文字，立即用 API tool/function-call channel 呼叫 %s。)",
			input.ToolName, input.Task, input.ToolName)
	case NudgeRunIntent:
		return "(你被要求「執行/跑」它，但這一輪沒有實際用 run_shell_command 去跑。" +
			"現在用 run_shell_command 真的執行，把真實輸出貼出來——不要自己用算的或假設結果。)"
	case NudgeTaskStart:
		return "(任務尚未真正開始。不要用文字描述你打算做什麼；現在呼叫最小且必要的工具，先取得真實狀態或直接建立要求的輸出。)"
	case NudgeTaskContinue:
		return "(要求的實際工作尚未完成。繼續使用合適的工具完成交付，不要只提供步驟、範例或宣稱已完成。)"
	case NudgeStrategyChange:
		failure := strings.TrimSpace(input.Failure)
		if failure != "" {
			failure = " 目前失敗訊號：" + failure + "。"
		}
		tool := strings.TrimSpace(input.ToolName)
		if tool != "" {
			tool = "（最近工具：" + tool + "）"
		}
		return "(目前方法沒有進展" + tool + "。" + failure +
			"失敗輸出已在目前 context；先分類失敗，做一次不同且有界的診斷，再採取較小的下一步。只有證據已證明 artifact 有具體缺陷時，下一個 native tool call 才應直接做針對性 edit/write。Schema、selector、coverage、assertion format 或 validator setup 錯誤屬於驗證器問題，不得拿來授權修改程式；修正驗證器，或把視覺玩法交還人類。不要重讀不變內容，也不要原樣重複失敗呼叫。不要用文字假裝成功。)"
	case NudgeEvidenceRequired:
		return "(目前缺少完成任務所需的真實證據。先用 read/search/list/web 工具取得實際內容或來源，再根據工具結果完成答案；不要依靠猜測。)"
	case NudgeEvidenceStatus:
		status := input.Verification
		if status == "" {
			status = VerificationUnverified
		}
		message := "(Meadow evidence gate：目前完成宣稱尚未獲得證據支持；verification=" + string(status) + "。"
		if strings.TrimSpace(input.Failure) == "last_mutation_failed" {
			message += " last_mutation=failed；之後的唯讀檢查不代表修改成功。"
		}
		if paths := compactResponsePaths(input.ChangedPaths, 4); paths != "" {
			message += " 已變更 " + paths + "。"
		}
		if command := strings.TrimSpace(input.ValidationCommand); command != "" {
			message += " 使用者或執行環境指定的驗證條件是 `" + command + "`。"
		}
		if (status == VerificationUnverified || status == VerificationStale) && len(input.ChangedPaths) > 0 {
			message += " 若使用 shell acceptance check 驗證修改，該工具呼叫必須帶 `purpose=validation`；單純讀回檔案或只做語法解析不算語義驗證。"
		}
		return message + " 請根據這些事實自行決定下一步、重新規劃、取得相關證據，或如實說明阻礙；Meadow 不替你選擇工具。)"
	case NudgeRequirementAudit:
		validator := strings.TrimSpace(input.ValidationCommand)
		phase := "目前已進入 audit phase；solve／repair／selected validation 都是已完成的前一階段。"
		if validator != "" {
			phase += " Selected validator `" + validator + "` 已對最新 artifact 通過。"
		}
		phase += " 原始需求中「第一步／先執行／先取得失敗」等流程順序，是前一階段已完成的歷史條件；不得在 audit phase 重新開始或把失敗碼列為成功。"
		phase += sourceBackedAuditNudge
		if input.PreserveHistory {
			return "(Independent requirement audit：" + phase + " 前面的解題與驗證歷史仍保留；請直接延續目前 artifact，不要重新開始流程或重播 selected validator。逐條檢查尚未被舊 validator 證明的明示需求、邊界與無效輸入行為。下一個回應不要解說，只呼叫一次 `run_shell_command`：`command` 恰好是 `python3`、checker 原始碼放入 `script`、`purpose=validation`。checker 必須是最多 40 行的 table-driven assertions；例外類型、排序鍵與 expected 值逐字依權威需求推導，不得沿用 artifact 的行為當答案。系統會建立並清除暫存 script，禁止用 `write_file` 在 workspace 建 checker。若 check non-zero，先對照權威需求判斷是 artifact 還是 checker 錯；只有兩個不同 checker 都直接證明 artifact 違反同一明示需求時，才以 `record_requirement_audit` 將該具體缺口標為 uncovered、逐字引用原始需求到 contract_quote，並引用兩個 fresh failing call ID。checker 自身載入、路徑、語法、fixture 或 expected 錯誤不算 artifact 缺口，應只修 checker 並重跑一次。下一輪才針對已登記缺口修正；若通過，以 covered 引用 fresh call ID 後結案。沒有已登記的 uncovered 失敗證據時不可修改已通過 artifact。)"
		}
		return "(Independent requirement audit：" + phase + " 既有驗證已通過，但先前推理與自我結論已從目前 context 移除。請從原始使用者需求、權威輸入與磁碟上的實際產物重新開始，逐條比對每一句明確需求、每個明確命名的輸出路徑、明示邊界條件與明示無效輸入行為。只審核使用者明示規格；不得擴張成新需求、額外修改或無止境測試。先讀取現有 artifact；沒有發現具體缺口時不可為了審核而重寫已通過的產物。數值或資料任務必須用新的 acceptance program 重新讀取權威輸入、獨立計算預期關係，再與 artifact 比對；不可重播審核前的 validator，也不可把目前 artifact 的行為複製成 expected 形成循環證明。讀取後的下一個回應不要解說，只呼叫一次 `run_shell_command`：`command` 恰好是 `python3`、新的 acceptance program 放入 `script`、`purpose=validation`。checker 必須是最多 40 行的 table-driven assertions，例外類型、排序鍵與 expected 值逐字依權威需求推導。系統會建立並清除暫存 script，禁止用 `write_file` 建 checker；該 check 在隔離 workspace generation 執行，不能修改已通過的真實 artifact。再呼叫 `record_requirement_audit`，用 covered／uncovered 與 evidence_call_ids 提交逐項證據。發現缺漏時，先核對 checker 是否忠實；只有兩個不同 checker 都直接證明 artifact 違反同一明示需求時才可登記 uncovered；必須逐字引用原始需求到 contract_quote 並引用兩個 fresh failing call ID。checker 自身載入、路徑、語法、fixture 或 expected 錯誤必須先修 checker。下一輪才會解鎖 artifact 修改；修改後必須重新跑 selected validator 和整份 audit。不要只讀回檔案或憑文字宣稱完整。)"
	case NudgeAuditEvidence:
		return "(Independent audit 尚未完成逐項需求證據。審核前的 passing validator 不可沿用或原樣重播。請從原始需求／權威輸入與目前 artifact，建立能在條件不成立時 non-zero exit 的新 acceptance check。下一個回應不要解說，只呼叫一次 `run_shell_command`：`command` 恰好是 `python3`，checker 放入 `script`，`purpose=validation`。checker 最多 40 行、用 table-driven assertions，例外類型、排序鍵與 expected 值逐字依權威需求推導；禁止用 `write_file` 建 checker。最後呼叫 `record_requirement_audit` 引用 fresh call ID，不要只輸出逐條打勾文字。)"
	case NudgePlanActive:
		return fmt.Sprintf(
			"(你自己建立的 task_plan 仍有 %d 個 pending 或 in_progress 項目，因此尚未完成。請自行繼續、重新規劃，或把已解決／不再需要的項目更新為 completed／cancelled；Meadow 不替你選擇下一個工具。)",
			input.ActivePlanItems)
	case NudgeOutputTruncated:
		return "(上一輪因模型輸出上限而被截斷，不能視為完成。不要重複或接著貼出長篇交付物；若任務要求建立或修改成果，請自行選擇合適工具實際完成。若只需回答，請精簡完成。)"
	case NudgeRepeatedTool:
		tool := strings.TrimSpace(input.ToolName)
		if tool != "" {
			tool = " tool=" + tool + "."
		}
		return "RECOVERY_REQUIRED: the last call repeated an unchanged observation and produced no new evidence." +
			tool + " Reuse the earlier result already in context. Your next tool call must use a different tool " +
			"or materially different arguments; otherwise return a truthful blocker. Do not repeat the same call."
	default:
		return ""
	}
}

// NudgePolicy owns per-run retry budgets for generic recovery signals. It does
// not compose prompts or inspect task content.
type NudgePolicy struct {
	limits   map[NudgeKind]int
	attempts map[NudgeKind]int
}

func DefaultNudgeLimits() map[NudgeKind]int {
	return map[NudgeKind]int{
		NudgeSyntaxRepair:      3,
		NudgeToolError:         2,
		NudgeValidationFailure: 4,
		NudgeStagedDeliverable: 1,
		NudgeNamedDeliverable:  2,
		NudgeForcedTool:        2,
		NudgeRunIntent:         1,
		NudgeTaskStart:         2,
		NudgeTaskContinue:      3,
		NudgeStrategyChange:    3,
		NudgeEvidenceRequired:  2,
		NudgeEvidenceStatus:    2,
		NudgeRequirementAudit:  1,
		NudgeAuditEvidence:     2,
		NudgePlanActive:        3,
		NudgeRepeatedTool:      1,
	}
}

func NewNudgePolicy(limits map[NudgeKind]int) *NudgePolicy {
	copyLimits := make(map[NudgeKind]int, len(limits))
	for kind, limit := range limits {
		if kind != "" && limit > 0 {
			copyLimits[kind] = limit
		}
	}
	return &NudgePolicy{limits: copyLimits, attempts: make(map[NudgeKind]int)}
}

func DefaultNudgePolicy() *NudgePolicy {
	return NewNudgePolicy(DefaultNudgeLimits())
}

func (p *NudgePolicy) Try(kind NudgeKind) NudgeDecision {
	if p == nil {
		return NudgeDecision{Kind: kind}
	}
	if p.attempts == nil {
		p.attempts = make(map[NudgeKind]int)
	}
	p.attempts[kind]++
	attempt := p.attempts[kind]
	limit := p.limits[kind]
	return NudgeDecision{Kind: kind, Allowed: limit > 0 && attempt <= limit, Attempt: attempt, Limit: limit}
}

func (p *NudgePolicy) Reset(kinds ...NudgeKind) {
	if p == nil {
		return
	}
	for _, kind := range kinds {
		delete(p.attempts, kind)
	}
}

func (p *NudgePolicy) Attempts(kind NudgeKind) int {
	if p == nil {
		return 0
	}
	return p.attempts[kind]
}
