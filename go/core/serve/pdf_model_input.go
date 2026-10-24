package serve

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

// Unknown capabilities keep the existing visual path and its fail-closed
// admission check. Only a known text-only model selects extracted PDF text.
func pdfTextOnlyModel(model string, clients ...any) bool {
	var client any
	if len(clients) > 0 {
		client = clients[0]
	}
	profile, known := provider.ProfileForClient(client, model)
	return known && !profile.Multimodal
}

func (sess *serveSession) attachmentModelID() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.engine == nil {
		return ""
	}
	return sess.engine.ModelID
}

func (sess *serveSession) attachmentClient() any {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.engine == nil {
		return nil
	}
	return sess.engine.Client
}

func (sess *serveSession) promptBlocks(raw []map[string]any) (provider.Message, error) {
	model := sess.attachmentModelID()
	msg, err := promptBlocksForModel(raw, model, sess.attachmentClient())
	if err == nil {
		err = validatePromptImages(msg, model, sess.attachmentClient())
	}
	return msg, err
}

func validateTextPDFAttachmentCount(raw []map[string]any) error {
	count, hasPDF := 0, false
	for _, block := range raw {
		switch stringValue(block["type"]) {
		case "file", "input_file":
			count++
			file, _ := block["file"].(map[string]any)
			hasPDF = hasPDF || strings.EqualFold(filepath.Ext(stringValue(file["name"])), ".pdf") || stringValue(file["media_type"]) == "application/pdf"
		case "image", "image_url", "input_image":
			count++
		}
	}
	if hasPDF && count > 4 {
		return fmt.Errorf("一次最多附 4 個檔案")
	}
	return nil
}

// Called only after source protection and server extraction, before indexing.
// PDF and Office previews share one budget regardless of their order.
func prepareTextPDFPreviews(raw []map[string]any) error {
	remaining := promptDocumentPreviewChars
	for _, block := range raw {
		_, _, document := promptDocumentBlock(block)
		name, _, pdf := promptPDFBlock(block)
		if !document && !pdf {
			continue
		}
		file := block["file"].(map[string]any)
		text := stringValue(file["extraction_text"])
		if pdf && (stringValue(file["pdf_text_status"]) != "available" || strings.TrimSpace(text) == "" || stringValue(file["extract_path"]) == "" || stringValue(file["source_sha256"]) == "") {
			return fmt.Errorf("PDF %s 沒有可用的抽取文字，無法交給目前的純文字模型；未執行 OCR，請改用支援圖片的 Vision 模型或提供可抽取文字的 PDF", name)
		}
		preview, truncated := boundedDocumentText(text, remaining)
		remaining -= utf8.RuneCountInString(preview)
		file["extracted_text"], file["preview_truncated"] = preview, truncated
		file["preview_budget_chars"], file["preview_budget_scope"] = promptDocumentPreviewChars, "all documents and text PDF attachments"
	}
	return nil
}

func promptPDFTextContext(block map[string]any) (string, error) {
	name, _, ok := promptPDFBlock(block)
	if !ok {
		return "", fmt.Errorf("PDF 附件格式無效")
	}
	file := block["file"].(map[string]any)
	preview, previewOK := file["extracted_text"].(string)
	if stringValue(file["pdf_text_status"]) != "available" || strings.TrimSpace(stringValue(file["extraction_text"])) == "" || !previewOK || stringValue(file["extract_path"]) == "" {
		return "", fmt.Errorf("PDF %s 尚未完成可用的文字抽取，無法交給純文字模型", name)
	}
	state := "complete"
	if bounded, _ := file["preview_truncated"].(bool); bounded {
		state = "bounded preview; remaining text is in extract_path"
	}
	return fmt.Sprintf("%s\nInput mode: PDF text extraction only; images, charts and visual layout were not observed. OCR was not performed.\nPreview: %s; preview budget %d characters shared by %s (overall document preview budget %d characters).\n--- BEGIN DOCUMENT DATA ---\n%s\n--- END DOCUMENT DATA ---", promptPDFDocumentContext(block), state, intValue(file["preview_budget_chars"]), stringValue(file["preview_budget_scope"]), promptDocumentPreviewChars, preview), nil
}
