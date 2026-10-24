package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/cli/theme"
	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/attachments"
	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/taskruntime"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

func formatBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

// menuKind says what selecting an item does.
type menuKind int

const (
	menuNone menuKind = iota
	menuTheme
	menuApproval
	menuResume
	menuTasks
	menuModel
	menuThink
	menuEffort
	menuCommand
)

type menuItem struct {
	label     string
	value     string
	desc      string
	sessionID string
}

type menuState struct {
	kind   menuKind
	title  string
	items  []menuItem
	cursor int
}

var slashCommands = []menuItem{
	{label: "/think", value: "/think", desc: "思考模式：auto / on / off"},
	{label: "/effort", value: "/effort", desc: "推理強度：low / medium / high"},
	{label: "/model", value: "/model", desc: "選擇並切換 Meadow 模型"},
	{label: "/search", value: "/search", desc: "開關資料搜集與 RAG 模式"},
	{label: "/attach", value: "/attach", desc: "附加圖片（/attach <圖片路徑>）"},
	{label: "/theme", value: "/theme", desc: "切換 CLI 彩色主題"},
	{label: "/two-pass", value: "/two-pass", desc: "切換二次推理"},
	{label: "/tasks", value: "/tasks", desc: "查看背景 Agent 與 Loop"},
	{label: "/approval", value: "/approval", desc: "切換工具授權模式"},
	{label: "/resume", value: "/resume", desc: "恢復先前對話"},
	{label: "/stats", value: "/stats", desc: "查看本次對話統計"},
	{label: "/clear", value: "/clear", desc: "清空畫面與對話歷史"},
	{label: "/help", value: "/help", desc: "顯示完整指令說明"},
	{label: "/quit", value: "/quit", desc: "離開 Meadow CLI"},
}

func slashCommandSuggestions(input string) menuState {
	prefix := strings.ToLower(strings.TrimSpace(input))
	if prefix == "" || !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, " \t\r\n") {
		return menuState{}
	}
	items := make([]menuItem, 0, 8)
	for _, item := range slashCommands {
		if strings.HasPrefix(strings.ToLower(item.value), prefix) {
			items = append(items, item)
			if len(items) == 8 {
				break
			}
		}
	}
	if len(items) == 0 {
		return menuState{}
	}
	return menuState{
		kind:  menuCommand,
		title: "指令建議（↑↓ 選擇、Enter 開啟、Tab 補全）",
		items: items,
	}
}

func themeMenu(current string) menuState {
	items := make([]menuItem, 0, len(theme.All))
	for _, t := range theme.All {
		mark := ""
		if t.Name == current {
			mark = "（目前）"
		}
		items = append(items, menuItem{label: t.Label + mark, value: t.Name})
	}
	return menuState{kind: menuTheme, title: "選擇主題（↑↓ 預覽、Enter 確定、Esc 取消）", items: items}
}

func approvalMenu(current string) menuState {
	descs := map[string]string{
		"plan":      "只讀不動手，提出計畫",
		"default":   "寫入與執行前詢問",
		"auto-edit": "自動允許檔案編輯，執行前詢問",
		"yolo":      "自動允許非破壞性操作；刪除與重設仍詢問",
	}
	var items []menuItem
	for _, m := range []string{"plan", "default", "auto-edit", "yolo"} {
		mark := ""
		if m == current {
			mark = "（目前）"
		}
		items = append(items, menuItem{label: m + mark, value: m, desc: descs[m]})
	}
	return menuState{kind: menuApproval, title: "Approval mode", items: items}
}

func modelMenu(current string, choices []ModelChoice) menuState {
	items := make([]menuItem, 0, len(choices))
	for _, choice := range choices {
		mark := ""
		if choice.ID == current {
			mark = "（目前）"
		}
		items = append(items, menuItem{label: choice.ID + mark, value: choice.ID, desc: choice.Description})
	}
	return menuState{kind: menuModel, title: "切換模型（會卸載目前模型並在同一對話繼續）", items: items}
}

func thinkingMenu(current provider.ThinkingMode) menuState {
	descs := map[provider.ThinkingMode]string{
		provider.ThinkingAuto: "使用模型專屬的穩定預設",
		provider.ThinkingOn:   "串流顯示模型原生推理；第一個推理 token 前仍需等待",
		provider.ThinkingOff:  "直接回答／執行，降低 TTFT 與推理 token",
	}
	items := make([]menuItem, 0, 3)
	for _, mode := range []provider.ThinkingMode{provider.ThinkingAuto, provider.ThinkingOn, provider.ThinkingOff} {
		mark := ""
		if mode == current {
			mark = "（目前）"
		}
		items = append(items, menuItem{label: string(mode) + mark, value: string(mode), desc: descs[mode]})
	}
	return menuState{kind: menuThink, title: "Thinking mode", items: items}
}

func effortMenu(current provider.ReasoningEffort) menuState {
	descs := map[provider.ReasoningEffort]string{
		provider.ReasoningEffortLow:    "較快、較少推理 token",
		provider.ReasoningEffortMedium: "平衡速度與解題深度",
		provider.ReasoningEffortHigh:   "較深推理、較慢且使用更多 token",
	}
	items := make([]menuItem, 0, 3)
	for _, effort := range []provider.ReasoningEffort{
		provider.ReasoningEffortLow, provider.ReasoningEffortMedium, provider.ReasoningEffortHigh,
	} {
		mark := ""
		if effort == current {
			mark = "（目前）"
		}
		items = append(items, menuItem{label: string(effort) + mark, value: string(effort), desc: descs[effort]})
	}
	return menuState{kind: menuEffort, title: "Reasoning effort（選擇後會同時開啟 think）", items: items}
}

func resumeMenu(projectRoot string) (menuState, error) {
	sessions, err := session.ListSessions(projectRoot)
	if err != nil {
		return menuState{}, err
	}
	var items []menuItem
	for _, s := range sessions {
		if len(items) >= 15 {
			break
		}
		items = append(items, menuItem{
			label:     s.Modified.Format("01-02 15:04") + "  " + oneLine(s.FirstUser, 48),
			value:     s.Path,
			sessionID: s.SessionID,
		})
	}
	if len(items) == 0 {
		return menuState{}, fmt.Errorf("這個專案還沒有儲存的對話")
	}
	return menuState{kind: menuResume, title: "恢復對話（Enter 載入、Esc 取消）", items: items}, nil
}

func buildTaskMenu(
	scheduler *taskruntime.Scheduler,
	background []tools.BackgroundTaskSnapshot,
	processes []tools.ProcessSnapshot,
	monitors []tools.MonitorSnapshot,
) menuState {
	menu := menuState{
		kind: menuTasks, title: "背景任務（Ctrl+G / Esc 關閉）",
	}
	for i := len(background) - 1; i >= 0 && len(menu.items) < 12; i-- {
		task := background[i]
		menu.items = append(menu.items, menuItem{
			label: fmt.Sprintf("%s  %s  %s", task.TaskID, task.Status, task.Kind),
			desc:  oneLine(task.Description, 76),
		})
	}
	monitorProcesses := map[string]bool{}
	for i := len(monitors) - 1; i >= 0 && len(menu.items) < 12; i-- {
		monitor := monitors[i]
		monitorProcesses[monitor.ProcessID] = true
		menu.items = append(menu.items, menuItem{
			label: fmt.Sprintf("%s  %s  monitor", monitor.ProcessID, monitor.Status),
			desc:  oneLine(monitor.Description, 76),
		})
	}
	for i := len(processes) - 1; i >= 0 && len(menu.items) < 12; i-- {
		process := processes[i]
		if monitorProcesses[process.ProcessID] {
			continue
		}
		menu.items = append(menu.items, menuItem{
			label: fmt.Sprintf("%s  %s  process", process.ProcessID, process.Status),
			desc:  oneLine(process.Command, 76),
		})
	}
	var tasks []taskruntime.Task
	if scheduler != nil {
		tasks = scheduler.List()
	}
	taskruntime.SortByNextRun(tasks)
	for _, task := range tasks {
		if len(menu.items) >= 12 {
			menu.items = append(menu.items, menuItem{
				label: "另有任務，使用 /tasks 或 task_output 查看",
			})
			break
		}
		status := string(task.Status)
		if task.Paused {
			status = string(taskruntime.StatusPaused)
		}
		menu.items = append(menu.items, menuItem{
			label: fmt.Sprintf("%s  %s  每 %s", task.ID, status, task.Interval),
			desc: fmt.Sprintf(
				"下次 %s · %s",
				task.NextRunAt.Local().Format("01-02 15:04:05"),
				oneLine(task.Prompt, 76),
			),
		})
	}
	if len(menu.items) == 0 {
		menu.items = []menuItem{{label: "目前沒有背景任務"}}
	}
	return menu
}

func renderMenu(m menuState, st theme.Styles) string {
	var b strings.Builder
	b.WriteString(st.Bright.Render(m.title) + "\n\n")
	for i, it := range m.items {
		line := "  " + it.label
		if i == m.cursor {
			line = st.Selected.Render("▸ " + it.label)
		}
		b.WriteString(line + "\n")
		if it.desc != "" {
			b.WriteString(st.Dim.Render("      "+it.desc) + "\n")
		}
	}
	return b.String()
}

const helpText = `指令
  /theme     切換主題（meadow / ayu / mist）
  /approval  切換 approval mode（plan / default / auto-edit / yolo）
  /resume    恢復這個專案先前的對話
  /two-pass  切換二次推理（先整理 Task Brief，再正式執行）
  /think     思考模式（auto / on / off）
  /effort    推理強度（low / medium / high；/effotr 亦可）
  /loop      建立可持久化的週期任務（例：/loop 5m 檢查測試）
  /tasks     列出背景 Agent 與 Loop 狀態
  /task-cancel <id>  取消背景 Agent 任務
  /loop-pause <id>   暫停 Loop
  /loop-resume <id>  恢復 Loop
  /loop-delete <id>  刪除 Loop
	/model     選擇模型；切換後在同一對話繼續
  /search    開關資料搜集模式（/search on、/search off）
  /stats     本次對話的 token 統計
  /clear     清空畫面與對話歷史
  /help      這個畫面
  /quit      離開

按鍵
  Enter      送出（訊息或選單確定）
  Alt+Enter  輸入框換行
  ↑↓         輸入框為空時捲動對話（有打字時才移動游標）
  滑鼠滾輪    捲動對話（要選取文字複製：MEADOW_TUI_MOUSE_TRACKING=off）
  Ctrl+U / Ctrl+D  半頁上/下（tmux 裡 PgUp 常被吃掉）
  End        跳到最新輸出（畫面卡住時按這個）
  Ctrl+G     顯示背景 Agent 與 Loop 任務
  y / a / n  工具確認：允許 / 一律允許 / 拒絕
  Ctrl+C     執行中=取消本輪；空輸入時=離開`

// dispatchSlash handles one /command. It returns a menu to open (kind !=
// menuNone), a system notice to print, or quit=true.
func (a *App) dispatchSlash(cmd string) (menuState, string, bool) {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) > 0 {
		switch fields[0] {
		case "/badAI", "/badai":
			a.editionSwitch = &EditionSwitchRequest{Profile: "skull"}
			a.quitting = true
			return menuState{}, "", true
		case "/goodAI", "/goodai":
			a.editionSwitch = &EditionSwitchRequest{Profile: "standard"}
			a.quitting = true
			return menuState{}, "", true
		case "/search":
			if len(fields) > 2 {
				return menuState{}, "用法：/search [on|off]", false
			}
			enable := a.runMode != agentcore.RunModeResearch
			if len(fields) == 2 {
				switch strings.ToLower(fields[1]) {
				case "on", "research":
					enable = true
				case "off", "auto":
					enable = false
				default:
					return menuState{}, "用法：/search [on|off]", false
				}
			}
			if enable {
				a.runMode = agentcore.RunModeResearch
				return menuState{}, "資料搜集模式已開啟：下一輪會先分析需求，再搜尋候選、用 Meadow Browser 讀取來源、建立 RAG，最後輸出帶引用的 Markdown 與 HTML；需要簡報時請在問題中指定。", false
			}
			a.runMode = agentcore.RunModeAuto
			return menuState{}, "資料搜集模式已關閉：恢復由模型判斷一般聊天、規劃或 Agent 任務。", false
		case "/attach", "/image":
			path := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmd), fields[0]))
			path = strings.Trim(path, "\"'")
			if path == "" {
				return menuState{}, "用法：/image <圖片路徑>（/attach 亦可）", false
			}
			if strings.HasPrefix(path, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
				}
			}
			if len(a.attachments) >= 4 {
				return menuState{}, "一次最多附 4 張圖片。", false
			}
			if profile, known := provider.ProfileForModel(a.binding.ModelID); known && !profile.Multimodal {
				return menuState{}, fmt.Sprintf("目前模型 %s 不支援圖片辨識；請切換到支援 vision 的模型後再附加。", a.binding.ModelID), false
			}
			normalized, err := attachments.NormalizeFile(path)
			if err != nil {
				return menuState{}, "圖片無法附加：" + err.Error(), false
			}
			a.attachments = append(a.attachments, provider.MessageContentPart{
				Type: "image_url", ImageURL: &provider.MessageImageURL{URL: normalized.DataURL},
			})
			return menuState{}, fmt.Sprintf("已附加圖片：%s（%dx%d，%s）", normalized.Name, normalized.Width, normalized.Height, formatBytes(normalized.DataBytes)), false
		case "/think":
			if len(fields) == 1 {
				return thinkingMenu(a.binding.thinkingMode()), "", false
			}
			if len(fields) != 2 {
				return menuState{}, "用法：/think [auto|on|off]", false
			}
			mode, err := provider.ParseThinkingMode(fields[1])
			if err != nil {
				return menuState{}, "用法：/think [auto|on|off]", false
			}
			if a.binding.SetThinkingMode == nil {
				return menuState{}, "目前 runtime 不支援 thinking 控制。", false
			}
			if err := a.binding.SetThinkingMode(mode); err != nil {
				return menuState{}, "thinking 無法切換：" + err.Error(), false
			}
			return menuState{}, thinkingModeNotice(mode, a.binding.reasoningEffort()), false
		case "/effort", "/effotr":
			if len(fields) == 1 {
				return effortMenu(a.binding.reasoningEffort()), "", false
			}
			if len(fields) != 2 {
				return menuState{}, "用法：/effort [low|medium|high]", false
			}
			effort, err := provider.ParseReasoningEffort(fields[1])
			if err != nil {
				return menuState{}, "用法：/effort [low|medium|high]", false
			}
			if a.binding.SetReasoningEffort == nil {
				return menuState{}, "目前 runtime 不支援 reasoning effort。", false
			}
			if err := a.binding.SetReasoningEffort(effort); err != nil {
				return menuState{}, "effort 無法切換：" + err.Error(), false
			}
			return menuState{}, effortNotice(effort), false
		case "/tasks":
			return menuState{}, a.formatAllTasks(), false
		case "/task-cancel":
			if len(fields) != 2 {
				return menuState{}, "用法：/task-cancel <task-id>", false
			}
			if strings.HasPrefix(fields[1], "proc_") {
				if a.binding.CancelProcess == nil {
					return menuState{}, "目前 runtime 不支援取消背景 process／monitor。", false
				}
				process, err := a.binding.CancelProcess(fields[1])
				if err != nil {
					return menuState{}, err.Error(), false
				}
				return menuState{}, fmt.Sprintf(
					"已停止 %s（目前狀態：%s）。",
					process.ProcessID, process.Status,
				), false
			}
			if a.binding.CancelBackgroundTask == nil {
				return menuState{}, "目前 runtime 不支援取消這個背景任務。", false
			}
			task, err := a.binding.CancelBackgroundTask(fields[1])
			if err != nil {
				return menuState{}, err.Error(), false
			}
			return menuState{}, fmt.Sprintf(
				"已要求取消 %s（目前狀態：%s）。",
				task.TaskID, task.Status,
			), false
		case "/loop-pause", "/loop-resume", "/loop-delete":
			if a.taskScheduler == nil {
				return menuState{}, "目前沒有 task scheduler。", false
			}
			if len(fields) != 2 {
				return menuState{}, "用法：" + fields[0] + " <task-id>", false
			}
			id := fields[1]
			switch fields[0] {
			case "/loop-pause":
				task, err := a.taskScheduler.Pause(id)
				if err != nil {
					return menuState{}, err.Error(), false
				}
				return menuState{}, fmt.Sprintf("已暫停 %s：%s", task.ID, oneLine(task.Prompt, 80)), false
			case "/loop-resume":
				task, err := a.taskScheduler.Resume(id)
				if err != nil {
					return menuState{}, err.Error(), false
				}
				return menuState{}, fmt.Sprintf(
					"已恢復 %s；下次執行 %s。",
					task.ID, task.NextRunAt.Local().Format("01-02 15:04:05"),
				), false
			case "/loop-delete":
				task, found, err := a.taskScheduler.Delete(id)
				if err != nil {
					return menuState{}, err.Error(), false
				}
				if !found {
					return menuState{}, "找不到 Loop：" + id, false
				}
				stopping := ""
				if a.activeTaskID == id && a.activeTaskCancel != nil {
					a.activeTaskCancel()
					stopping = "；正在停止目前這一輪"
				}
				return menuState{}, fmt.Sprintf(
					"已刪除 %s：%s%s",
					task.ID, oneLine(task.Prompt, 80), stopping,
				), false
			}
		}
	}
	switch strings.TrimSpace(cmd) {
	case "/help":
		return menuState{}, helpText, false
	case "/quit", "/exit":
		return menuState{}, "", true
	case "/clear":
		a.entries = []entry{a.bannerEntry()}
		a.history = nil
		a.attachments = nil
		a.lastPromptTokens = 0
		return menuState{}, "已清空對話。", false
	case "/theme":
		return themeMenu(a.theme.Name), "", false
	case "/approval":
		return approvalMenu(string(a.binding.mode())), "", false
	case "/model":
		if len(a.modelChoices) == 0 {
			return menuState{}, fmt.Sprintf("模型：%s\n端點：%s", a.binding.ModelID, a.baseURL), false
		}
		return modelMenu(a.binding.ModelID, a.modelChoices), "", false
	case "/stats":
		return menuState{}, fmt.Sprintf("本次對話:%d 輪、%d 次工具呼叫、上下文 %s；think=%s、effort=%s。",
			a.totalTurns, a.totalToolCalls, a.contextUsageLabel(),
			a.binding.thinkingMode(), a.binding.reasoningEffort()), false
	case "/resume":
		m, err := resumeMenu(a.projectRoot)
		if err != nil {
			return menuState{}, err.Error(), false
		}
		return m, "", false
	case "/two-pass", "/twopass":
		if a.binding.SetTwoPass == nil {
			return menuState{}, "目前 runtime 不支援二次推理。", false
		}
		enabled := !a.binding.twoPass()
		if err := a.binding.SetTwoPass(enabled); err != nil {
			return menuState{}, "二次推理無法切換：" + err.Error(), false
		}
		if enabled {
			return menuState{}, "二次推理已開啟：每次送出會先產生隱藏 Task Brief，再正式執行。", false
		}
		return menuState{}, "二次推理已關閉：回到單次推理。", false
	default:
		return menuState{}, "未知指令：" + cmd + "（/help 看全部）", false
	}
}

func thinkingModeNotice(mode provider.ThinkingMode, effort provider.ReasoningEffort) string {
	switch mode {
	case provider.ThinkingOn:
		return fmt.Sprintf("thinking 已開啟，reasoning effort=%s；下一輪會顯示模型實際送出的推理。第一個推理 token 前仍可能等待。", effort)
	case provider.ThinkingOff:
		return "thinking 已關閉；下一輪會直接回答或執行。"
	default:
		return "thinking 已恢復 auto；下一輪使用模型專屬的穩定預設。"
	}
}

func effortNotice(effort provider.ReasoningEffort) string {
	return fmt.Sprintf("reasoning effort 已切換為 %s，並開啟 thinking；下一輪開始生效。", effort)
}
