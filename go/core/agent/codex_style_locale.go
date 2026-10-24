package agent

import (
	"strings"
	"unicode"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// Attach after transport/schema assembly, in the stable leading system prefix.
// This is a generation contract, not output translation or a synthetic user task.
const codexStyleOutputLanguageContract = "【作業輸出語言契約】除非使用者明確要求另一種語言，這次生成的進度說明、分析摘要、修正理由、計畫、審查與最後回覆一律使用臺灣繁體中文。工具結果或規格是英文不代表應改用英文回覆。工具名稱、參數、程式碼、原始錯誤與引文維持原文；不要翻譯或改寫證據。"

func codexStyleAppendOutputLanguage(messages []provider.Message) []provider.Message {
	return codexStyleAppendTransportInstruction(messages, codexStyleOutputLanguageContract)
}

func codexStyleDefaultsToTaiwanTraditional(message provider.Message) bool {
	text := strings.TrimSpace(provider.MessageText(message))
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"zh-cn", "zh_cn", "simplified chinese", "简体中文", "簡體中文", "簡体中文",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	if text == "" {
		for _, part := range message.ContentParts {
			if part.ImageURL != nil && strings.TrimSpace(part.ImageURL.URL) != "" {
				return true
			}
		}
		return false
	}
	for _, char := range text {
		if (char >= '\u3040' && char <= '\u30ff') || (char >= '\u31f0' && char <= '\u31ff') {
			return false
		}
	}
	for _, char := range text {
		if unicode.Is(unicode.Han, char) {
			return true
		}
	}
	return false
}
