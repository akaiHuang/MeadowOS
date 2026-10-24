package cli

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/hey-meadow/meadow-code/go/cli/theme"
	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const (
	standardActivityRowCount    = 3
	largeInputActivityRowCount  = 4
	activitySweepWidth          = 5
	activityStallWarn           = 20 * time.Second
	largeInputReadingThreshold  = 32_000
	largeInputColdPrefillTokens = 232
)

// activityLineAt renders a truthful heartbeat immediately after the latest
// transcript event. Ordinary work keeps the established three rows; a large
// uncached prefill gets one additional local reading-excerpt row.
func (a *App) activityLineAt(now time.Time) string {
	runElapsed := now.Sub(a.runStart)
	if runElapsed < 0 {
		runElapsed = 0
	}
	ticks := int(runElapsed / activityTickInterval)
	spin := spinnerFrames[ticks%len(spinnerFrames)]
	if a.largeInputReadingActive() {
		return a.largeInputActivityLine(now, spin, ticks, runElapsed)
	}
	detail := a.activityDetail(runElapsed)

	// The detail owns its row now, so it can use most of the terminal width.
	// On narrow terminals it still contracts before the elapsed/cancel suffix.
	detailWidth := max(a.width-28, 18)
	detail = oneLine(detail, detailWidth)
	suffix := fmt.Sprintf(" · %s", runElapsed.Round(time.Second))
	if a.width <= 0 || a.width >= 66 {
		suffix += " · Ctrl+C 取消"
	}

	spinner := a.styles.Accent.Render(spin + " ")
	return strings.Join([]string{
		spinner + activitySweep(a.theme, ticks),
		spinner + a.styles.Secondary.Render(waitingKaomoji(ticks)+" "+runningLabel(ticks)),
		a.styles.Accent.Render(spin + " " + detail + suffix),
	}, "\n")
}

func (a *App) activityRowCount() int {
	if a.largeInputReadingActive() {
		return largeInputActivityRowCount
	}
	return standardActivityRowCount
}

func (a *App) largeInputReadingActive() bool {
	progress := a.inferenceProgress
	if progress != nil {
		switch progress.Phase {
		case provider.InferenceFirstToken, provider.InferenceDecoding,
			provider.InferenceCompleted, provider.InferenceFailed:
			return false
		}
		if progress.PrefillTotalTokens > 0 {
			// A large cached conversation with only a short new suffix is not a
			// large read. Never replay cached text as if the model read it again.
			return progress.PrefillTotalTokens >= largeInputReadingThreshold
		}
		if progress.RemainingTokens > 0 {
			return progress.RemainingTokens >= largeInputReadingThreshold
		}
		if progress.PromptEstimatedTokens > 0 {
			return progress.PromptEstimatedTokens >= largeInputReadingThreshold
		}
	}
	return a.activePromptTokens >= largeInputReadingThreshold
}

func (a *App) largeInputActivityLine(
	now time.Time,
	spin string,
	ticks int,
	runElapsed time.Duration,
) string {
	spinner := a.styles.Accent.Render(spin + " ")
	rowWidth := max(a.width-2, 18)
	intro := a.largeInputIntro()
	excerpt := a.largeInputExcerpt(now, rowWidth)
	detail := a.largeInputProgressDetail(runElapsed)
	return strings.Join([]string{
		spinner + activitySweep(a.theme, ticks),
		a.styles.Accent.Render(spin + " " + oneLine(intro, rowWidth-2)),
		a.styles.Accent.Render(spin + " " + oneLine(excerpt, rowWidth-2)),
		a.styles.Accent.Render(spin + " " + oneLine(detail, rowWidth-2)),
	}, "\n")
}

func (a *App) largeInputIntro() string {
	tokens := a.activePromptTokens
	if a.inferenceProgress != nil {
		if exact := a.inferenceProgress.PrefillTotalTokens; exact > 0 {
			tokens = exact
		} else if exact := a.inferenceProgress.RemainingTokens; exact > 0 {
			tokens = exact
		} else if estimate := a.inferenceProgress.PromptEstimatedTokens; estimate > 0 {
			tokens = estimate
		}
	}
	if tokens <= 0 {
		return "嗯，這份資料有點多 · 正在估算完整閱讀時間"
	}
	estimate := time.Duration(
		math.Ceil(float64(tokens)/largeInputColdPrefillTokens),
	) * time.Second
	return fmt.Sprintf(
		"嗯，這份資料有點多 · 約 %s tokens · 完整咀嚼約 %s",
		humanTokens(tokens),
		humanDurationRange(estimate),
	)
}

func (a *App) largeInputProgressDetail(runElapsed time.Duration) string {
	progress := a.inferenceProgress
	if progress != nil && (progress.Phase == provider.InferenceDocumentIngest ||
		progress.Phase == provider.InferenceDocumentReady) {
		if progress.Phase == provider.InferenceDocumentReady {
			verb := "索引完成"
			if progress.IngestionReused {
				verb = "已重用相同資料索引"
			}
			return fmt.Sprintf(
				"%s · %d chunks · %s · 準備交給 Agent",
				verb, progress.IngestionTotal, runElapsed.Round(time.Second),
			)
		}
		return fmt.Sprintf(
			"建立索引 %d/%d chunks · %s/%s · %s · 可隨時補充方向",
			progress.IngestionProcessed, progress.IngestionTotal,
			humanIngestionBytes(progress.IngestionProcessedBytes),
			humanIngestionBytes(progress.IngestionTotalBytes),
			runElapsed.Round(time.Second),
		)
	}
	if progress == nil || progress.PrefillTotalTokens <= 0 {
		return fmt.Sprintf(
			"準備逐段閱讀 · %s · 可隨時補充方向 · Ctrl+C 取消",
			runElapsed.Round(time.Second),
		)
	}
	detail := fmt.Sprintf(
		"已閱讀 %s/%s · %s",
		humanTokens(progress.PrefillProcessedTokens),
		humanTokens(progress.PrefillTotalTokens),
		runElapsed.Round(time.Second),
	)
	if progress.PrefillObservedTokens <= 0 || progress.PrefillObservedElapsed <= 0 {
		detail += " · 正在校準閱讀速度"
	} else if eta := prefillETA(*progress); eta > 0 {
		detail += " · 預估剩餘 " + eta.String()
	}
	if a.width <= 0 || a.width >= 66 {
		detail += " · Ctrl+C 取消"
	}
	return detail
}

func (a *App) largeInputExcerpt(now time.Time, width int) string {
	progress := a.inferenceProgress
	if progress != nil && (progress.Phase == provider.InferenceDocumentIngest ||
		progress.Phase == provider.InferenceDocumentReady) {
		if progress.Phase == provider.InferenceDocumentReady {
			return "文件索引 · " + progress.IngestionSourceID + " · 所有片段都有來源座標"
		}
		coordinate := progress.IngestionSourceID
		if progress.IngestionStartLine > 0 {
			coordinate += fmt.Sprintf(":L%d-L%d", progress.IngestionStartLine, progress.IngestionEndLine)
		}
		note := strings.TrimSpace(progress.IngestionNote)
		if note == "" {
			return "原文摘記 · " + coordinate + " · 正在提交下一個片段"
		}
		return "原文摘記 · " + coordinate + "「" + truncateActivityRunes(note, max(width/2, 24)) + "」"
	}
	if progress == nil || progress.Phase != provider.InferencePrefill ||
		progress.PrefillTotalTokens <= 0 {
		return "閱讀片段 · 等待開始處理；你可以直接補充閱讀方向"
	}
	if progress.ReadingExcerpt == "" {
		source := strings.TrimSpace(progress.ReadingSource)
		if source == "" {
			source = "固定規則與前段對話"
		}
		return "閱讀片段 · 正在處理" + source
	}
	segment := []rune(progress.ReadingExcerpt)
	previewRunes := max(width/2, 24)
	if len(segment) > previewRunes {
		segment = segment[len(segment)-previewRunes:]
	}
	reveal := 8
	if !a.lastLiveProgress.IsZero() && now.After(a.lastLiveProgress) {
		reveal += int(now.Sub(a.lastLiveProgress) / (40 * time.Millisecond))
	}
	reveal = min(reveal, len(segment))
	if reveal <= 0 {
		return "閱讀片段 · 正在前進到下一段"
	}
	source := strings.TrimSpace(progress.ReadingSource)
	if source == "" {
		source = "資料"
	}
	return "閱讀片段 · " + source + "「" + string(segment[:reveal]) + "」"
}

func truncateActivityRunes(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func humanIngestionBytes(value int) string {
	if value < 1024 {
		return fmt.Sprintf("%dB", value)
	}
	if value < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(value)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(value)/(1024*1024))
}

func humanDurationRange(estimate time.Duration) string {
	if estimate <= 0 {
		return "估算中"
	}
	if estimate < 90*time.Second {
		lower := max(int(math.Floor(estimate.Seconds()*0.8)), 1)
		upper := max(int(math.Ceil(estimate.Seconds()*1.2)), lower+1)
		return fmt.Sprintf("%d–%d 秒", lower, upper)
	}
	lower := max(int(math.Floor(estimate.Minutes()*0.9)), 1)
	upper := max(int(math.Ceil(estimate.Minutes()*1.1)), lower+1)
	return fmt.Sprintf("%d–%d 分鐘", lower, upper)
}

func (a *App) activityDetail(runElapsed time.Duration) string {
	detail := ""
	if a.draftName != "" {
		// Tool argument bytes are real streamed progress. This is especially
		// useful while the model is producing a complete file for write_file.
		detail = fmt.Sprintf("正在準備 %s · %s", a.draftName, humanBytes(a.draftBytes))
	} else if active, count := a.activeTool(); active != nil {
		label := active.toolName
		if active.toolSummary != "" {
			label += " · " + oneLine(active.toolSummary, 48)
		}
		if count > 1 {
			label += fmt.Sprintf("（另有 %d 個工具）", count-1)
		}
		detail = "正在執行 " + label
	} else if a.inferenceProgress != nil {
		detail = inferenceProgressDetail(*a.inferenceProgress)
		if a.recoveryPending {
			detail = "具體錯誤已回傳模型 · " + detail
		}
	} else if strings.TrimSpace(a.streaming) != "" {
		detail = fmt.Sprintf("第 %d 輪 · 正在產生回覆", a.totalTurns+1)
	} else if a.validationPassed {
		detail = "檢查已通過 · 正在整理結果"
	} else if a.recoveryPending {
		detail = "具體錯誤已回傳模型 · 等待下一步"
		if a.recoveryTool != "" {
			detail = a.recoveryTool + " 錯誤已回傳模型 · 等待下一步"
		}
	} else {
		detail = fmt.Sprintf("第 %d 輪 · %s", a.totalTurns+1, waitingPhrase(runElapsed))
	}
	pendingGuidance := 0
	for i := range a.entries {
		if a.entries[i].kind == entrySteer &&
			a.entries[i].guidanceDisposition == string(agentcore.GuidanceAccepted) {
			pendingGuidance++
		}
	}
	if pendingGuidance > 0 {
		detail += fmt.Sprintf(" · %d 則引導待套用", pendingGuidance)
	}
	if len(a.queuedInputs) > 0 {
		detail += fmt.Sprintf(" · %d 題排隊", len(a.queuedInputs))
	}
	if !a.lastLiveProgress.IsZero() {
		now := a.runStart.Add(runElapsed)
		if stalled := now.Sub(a.lastLiveProgress); stalled >= activityStallWarn {
			detail += fmt.Sprintf(" · ⚠ %s 無新進度", stalled.Round(time.Second))
		}
	}
	return detail
}

func inferenceProgressDetail(progress provider.InferenceProgress) string {
	model := strings.TrimPrefix(progress.Model, "Meadow ")
	if model == "" {
		model = progress.Model
	}
	switch progress.Phase {
	case provider.InferenceConnecting:
		return "送往 Meadow " + model
	case provider.InferenceModelLoading:
		return "載入 Meadow " + model
	case provider.InferenceModelReady:
		return "Meadow " + model + " 已載入"
	case provider.InferenceAgentAudit:
		return "逐項核對需求與獨立驗證"
	case provider.InferenceAgentCheckpoint:
		return "保存目前最佳可驗證版本"
	case provider.InferenceAgentRecovery:
		return "前一策略無進展 · 正在更換解法"
	case provider.InferenceAgentRollback:
		return "回復目前最佳版本"
	case provider.InferenceAgentVerifying:
		return "重新驗證回復版本"
	case provider.InferenceAgentValidationFailed:
		return "驗收未通過 · 已把具體錯誤交回模型"
	case provider.InferenceAgentValidationPassed:
		return "指定驗收已通過 · 準備獨立核對"
	case provider.InferenceQueued:
		return "等待 Meadow Core 排程"
	case provider.InferenceCacheLookup:
		result := strings.ToUpper(progress.CacheHitType)
		if result == "" {
			result = "LOOKUP"
		}
		return fmt.Sprintf(
			"Prefix cache %s · %s cached · %d new",
			result,
			humanTokens(progress.CachedTokens),
			progress.RemainingTokens,
		)
	case provider.InferencePrefill:
		if progress.CacheHitType != "" && progress.CachedTokens > 0 {
			result := strings.ToUpper(progress.CacheHitType)
			detail := fmt.Sprintf(
				"Prefix cache %s · %s cached · %d new · prefill %s/%s",
				result,
				humanTokens(progress.CachedTokens),
				progress.RemainingTokens,
				humanTokens(progress.PrefillProcessedTokens),
				humanTokens(progress.PrefillTotalTokens),
			)
			return detail + prefillETADetail(progress)
		}
		if progress.PrefillTotalTokens > 0 {
			detail := fmt.Sprintf(
				"處理新輸入 tokens · %s/%s",
				humanTokens(progress.PrefillProcessedTokens),
				humanTokens(progress.PrefillTotalTokens),
			)
			return detail + prefillETADetail(progress)
		}
		return fmt.Sprintf("處理 %s 個新輸入 tokens", humanTokens(progress.RemainingTokens))
	case provider.InferenceFirstToken:
		if progress.IncludesActivation {
			return fmt.Sprintf(
				"First output · %.2fs（含模型載入）",
				progress.Elapsed.Seconds(),
			)
		}
		if !progress.FirstTokenVisible {
			return fmt.Sprintf("Model first token · %.2fs", progress.Elapsed.Seconds())
		}
		return fmt.Sprintf("First token · %.2fs", progress.Elapsed.Seconds())
	case provider.InferenceDecoding:
		parts := []string{"正在產生回覆"}
		if progress.CompletionTokens > 0 {
			parts = append(parts, humanTokens(progress.CompletionTokens)+" tokens")
			decodeElapsed := progress.Elapsed - progress.TTFT
			if decodeElapsed > 0 {
				parts = append(
					parts,
					fmt.Sprintf(
						"%.1f tok/s",
						float64(progress.CompletionTokens)/decodeElapsed.Seconds(),
					),
				)
			}
		}
		if progress.TTFT > 0 {
			if !progress.FirstTokenVisible {
				parts = append(parts, fmt.Sprintf("Model TTFT %.2fs", progress.TTFT.Seconds()))
			} else {
				parts = append(parts, fmt.Sprintf("TTFT %.2fs", progress.TTFT.Seconds()))
			}
		}
		return strings.Join(parts, " · ")
	case provider.InferenceCompleted:
		return "生成完成 · 正在整理結果"
	case provider.InferenceFailed:
		return "模型請求失敗 · 正在整理錯誤"
	default:
		return fmt.Sprintf("第 1 輪 · %s", waitingPhrase(progress.Elapsed))
	}
}

func prefillETADetail(progress provider.InferenceProgress) string {
	estimate := prefillETA(progress)
	if estimate <= 0 {
		return ""
	}
	return " · 預估剩餘 " + estimate.String()
}

func prefillETA(progress provider.InferenceProgress) time.Duration {
	remaining := progress.PrefillTotalTokens - progress.PrefillProcessedTokens
	if remaining <= 0 {
		return 0
	}
	processed := progress.PrefillProcessedTokens
	elapsed := progress.Elapsed
	if progress.PrefillObservedTokens > 0 && progress.PrefillObservedElapsed > 0 {
		processed = progress.PrefillObservedTokens
		elapsed = progress.PrefillObservedElapsed
	}
	if processed <= 0 || elapsed <= 0 {
		return 0
	}
	estimate := time.Duration(
		float64(elapsed) * float64(remaining) / float64(processed),
	).Round(time.Second)
	if estimate < time.Second {
		estimate = time.Second
	}
	return estimate
}

func humanTokens(tokens int) string {
	if tokens >= 1000 {
		return fmt.Sprintf("%.1fK", float64(tokens)/1000)
	}
	return fmt.Sprintf("%d", tokens)
}

// activitySweep is a terminal-friendly gradient scan. It uses the active
// theme's dim, secondary, and accent colors instead of hard-coded RGB values,
// so Meadow, Ayu, and Mist all retain their visual identity.
func activitySweep(th theme.Theme, phase int) string {
	var b strings.Builder
	for i := 0; i < activitySweepWidth; i++ {
		color := activitySweepColor(th, phase, i)
		b.WriteString(lipgloss.NewStyle().Foreground(color).Render("━"))
	}
	return b.String()
}

func activitySweepColor(th theme.Theme, phase, index int) lipgloss.Color {
	// Move in one direction only. Circular trailing distance keeps the glow
	// continuous as the hotspot wraps from the right edge back to the left.
	hotspot := phase % activitySweepWidth
	distance := (hotspot - index + activitySweepWidth) % activitySweepWidth
	switch distance {
	case 0:
		return th.Accent
	case 1:
		return th.Secondary
	case 2:
		return th.TextDim
	default:
		return th.BorderDim
	}
}
