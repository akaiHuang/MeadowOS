package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/session"
)

const (
	promptPDFMaxPages     = 16
	promptPDFMaxLongEdge  = 1600
	promptPDFJPEGQuality  = 82
	promptPDFPageMaxBytes = 1 << 20
)

type renderedPDFPage struct {
	Number    int
	Count     int
	MediaType string
	FilePath  string
}

func addPromptPDFVisualPages(raw []map[string]any) (result []map[string]any, err error) {
	result = make([]map[string]any, 0, len(raw))
	createdDirs := make([]string, 0)
	remainingPages := promptPDFMaxPages
	defer func() {
		if err == nil {
			return
		}
		for _, dir := range createdDirs {
			_ = os.RemoveAll(dir)
		}
	}()
	for _, block := range raw {
		copyBlock := clonePromptBlock(block)
		name, path, ok := promptPDFBlock(copyBlock)
		if !ok {
			result = append(result, copyBlock)
			continue
		}
		if remainingPages <= 0 {
			return nil, fmt.Errorf("一次最多直接觀看 %d 頁 PDF", promptPDFMaxPages)
		}
		pages, err := renderPDFPages(path, remainingPages, promptPDFMaxLongEdge, promptPDFJPEGQuality)
		if err != nil {
			return nil, fmt.Errorf("無法將 PDF %s 轉成頁面影像：%w", name, err)
		}
		if len(pages) == 0 {
			return nil, fmt.Errorf("PDF %s 沒有可辨識的頁面", name)
		}
		remainingPages -= len(pages)
		createdDirs = append(createdDirs, filepath.Dir(pages[0].FilePath))
		visualPages := make([]map[string]any, 0, len(pages))
		for _, page := range pages {
			if err := validatePromptPDFPagePath(page.FilePath); err != nil {
				return nil, err
			}
			visualPages = append(visualPages, map[string]any{
				"page": page.Number, "page_count": page.Count,
				"media_type": page.MediaType, "file_path": page.FilePath,
			})
		}
		file := copyBlock["file"].(map[string]any)
		file["visual_pages"] = visualPages
		copyBlock["file"] = file
		result = append(result, copyBlock)
	}
	return result, nil
}

func preparePromptPDFs(raw []map[string]any) ([]map[string]any, error) {
	materialized, err := materializePromptPDFs(raw)
	if err != nil {
		return nil, err
	}
	prepared, err := addPromptPDFVisualPages(materialized)
	if err != nil {
		cleanupPromptPDFTemps(materialized)
		return nil, err
	}
	return prepared, nil
}

// cleanupPromptPDFTemps releases Core-owned render directories and PDF source
// snapshots after the provider message has captured the page JPEGs as data
// URLs. Native Desktop drop directories remain owned by Desktop so a validation
// error does not invalidate the attachment card before a retry.
func cleanupPromptPDFTemps(raw []map[string]any) {
	removed := map[string]struct{}{}
	removeOwnedDir := func(path string) {
		dir := filepath.Clean(filepath.Dir(strings.TrimSpace(path)))
		base := filepath.Base(dir)
		if !strings.HasPrefix(base, "meadow-pdf-render-") &&
			!strings.HasPrefix(base, "meadow-pdf-attachment-") {
			return
		}
		if _, exists := removed[dir]; exists {
			return
		}
		removed[dir] = struct{}{}
		_ = os.RemoveAll(dir)
	}
	for _, block := range raw {
		_, source, ok := promptPDFBlock(block)
		if !ok {
			continue
		}
		removeOwnedDir(source)
		file, _ := block["file"].(map[string]any)
		if file == nil {
			continue
		}
		switch pages := file["visual_pages"].(type) {
		case []map[string]any:
			for _, page := range pages {
				removeOwnedDir(stringValue(page["file_path"]))
			}
		case []any:
			for _, rawPage := range pages {
				page, _ := rawPage.(map[string]any)
				removeOwnedDir(stringValue(page["file_path"]))
			}
		}
	}
}

// persistQueuedPromptPDFs moves queue-owned PDF sources out of /tmp so a
// pending prompt remains executable after Core or the Mac restarts. Rendered
// JPEGs are intentionally not stored in the queue JSON; they are recreated
// from this durable source immediately before the model turn.
func persistQueuedPromptPDFs(cwd, chatID string, raw []map[string]any) ([]map[string]any, error) {
	dir, err := session.ArtifactsDir(cwd, chatID)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(raw))
	for _, block := range raw {
		copyBlock := clonePromptBlock(block)
		if _, format, ok := promptDocumentBlock(copyBlock); ok {
			file := copyBlock["file"].(map[string]any)
			if _, err := readSessionDocument(cwd, chatID, stringValue(file["file_path"]), format.Extension); err != nil {
				return nil, err
			}
			clearPromptDocumentRuntime(file)
			result = append(result, copyBlock)
			continue
		}
		name, source, ok := promptPDFBlock(copyBlock)
		if !ok {
			result = append(result, copyBlock)
			continue
		}
		if validateQueuedPromptPDFPath(cwd, chatID, source) == nil {
			file := copyBlock["file"].(map[string]any)
			delete(file, "visual_pages")
			clearPromptDocumentRuntime(file)
			result = append(result, copyBlock)
			continue
		}
		data, readErr := os.ReadFile(source)
		if readErr != nil || len(data) < 5 || len(data) > promptPDFMaxBytes || string(data[:5]) != "%PDF-" {
			return nil, fmt.Errorf("PDF %s 無法建立耐久排隊副本", name)
		}
		target := filepath.Join(dir, "queued-pdf-"+session.NewUUID()+".pdf")
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return nil, fmt.Errorf("PDF %s 無法建立耐久排隊副本", name)
		}
		file := copyBlock["file"].(map[string]any)
		file["file_path"] = target
		delete(file, "visual_pages")
		clearPromptDocumentRuntime(file)
		result = append(result, copyBlock)
	}
	return result, nil
}

func prepareQueuedPromptPDFs(cwd, chatID string, raw []map[string]any) ([]map[string]any, error) {
	return prepareQueuedPromptPDFsForModel(cwd, chatID, raw, "")
}

func prepareQueuedPromptPDFsForModel(cwd, chatID string, raw []map[string]any, model string, clients ...any) ([]map[string]any, error) {
	textOnly := pdfTextOnlyModel(model, clients...)
	if textOnly {
		if err := validateTextPDFAttachmentCount(raw); err != nil {
			return nil, err
		}
	}
	for _, block := range raw {
		name, path, ok := promptPDFBlock(block)
		if !ok {
			continue
		}
		if err := validateQueuedPromptPDFPath(cwd, chatID, path); err != nil {
			return nil, fmt.Errorf("排隊 PDF %s 已遺失或未經授權，請重新附加檔案", name)
		}
	}
	prepared, _, err := preparePromptDocuments(cwd, chatID, raw, true)
	if err != nil {
		return nil, err
	}
	if textOnly {
		// Queue metadata is not authoritative; no prior visual/runtime fields
		// select the current model's input. Text is re-extracted afterwards.
		for _, block := range prepared {
			if _, _, pdf := promptPDFBlock(block); pdf {
				file := block["file"].(map[string]any)
				delete(file, "visual_pages")
				clearPromptDocumentRuntime(file)
			}
		}
		return prepared, nil
	}
	return addPromptPDFVisualPages(prepared)
}

func validateQueuedPromptPDFPath(cwd, chatID, path string) error {
	_, err := readSessionPDFDocument(cwd, chatID, path)
	return err
}

func promptPDFVisualPages(block map[string]any) ([]renderedPDFPage, error) {
	name, _, ok := promptPDFBlock(block)
	if !ok {
		return nil, nil
	}
	file, _ := block["file"].(map[string]any)
	rawPages, exists := file["visual_pages"]
	if !exists {
		return nil, fmt.Errorf("PDF %s 尚未建立視覺頁面，請重新附加檔案", name)
	}
	items, ok := rawPages.([]any)
	if !ok {
		if typed, typedOK := rawPages.([]map[string]any); typedOK {
			items = make([]any, len(typed))
			for i := range typed {
				items[i] = typed[i]
			}
		} else {
			return nil, fmt.Errorf("PDF %s 的視覺頁面格式無效", name)
		}
	}
	if len(items) == 0 || len(items) > promptPDFMaxPages {
		return nil, fmt.Errorf("PDF %s 的視覺頁面數無效", name)
	}
	pages := make([]renderedPDFPage, 0, len(items))
	for index, item := range items {
		page, _ := item.(map[string]any)
		if page == nil {
			return nil, fmt.Errorf("PDF %s 的第 %d 頁格式無效", name, index+1)
		}
		path := strings.TrimSpace(stringValue(page["file_path"]))
		if err := validatePromptPDFPagePath(path); err != nil {
			return nil, err
		}
		number := intValue(page["page"])
		count := intValue(page["page_count"])
		if number != index+1 || count != len(items) {
			return nil, fmt.Errorf("PDF %s 的頁碼資料無效", name)
		}
		pages = append(pages, renderedPDFPage{
			Number: number, Count: count, MediaType: "image/jpeg", FilePath: path,
		})
	}
	return pages, nil
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func validatePromptPDFPagePath(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	tempRoot := filepath.Clean(os.TempDir())
	rel, err := filepath.Rel(tempRoot, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("PDF 頁面影像路徑未經授權")
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(path)), "meadow-pdf-render-") {
		return fmt.Errorf("PDF 頁面影像路徑未經授權")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > promptPDFPageMaxBytes {
		return fmt.Errorf("PDF 頁面影像無法讀取或超過 1 MB")
	}
	return nil
}
