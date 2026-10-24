package serve

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/attachments"
)

const promptPDFMaxBytes = 64 << 20

// materializePromptPDFs converts browser-owned PDF data URLs into private
// local files before the visual renderer reads them. Native Desktop drops
// already arrive as private temp paths. PDF bytes and paths never enter the
// language-model context; preparePromptPDFs renders the pages as images.
func materializePromptPDFs(raw []map[string]any) (result []map[string]any, err error) {
	result = make([]map[string]any, 0, len(raw))
	owned := make([]map[string]any, 0)
	defer func() {
		if err != nil {
			cleanupPromptPDFTemps(owned)
		}
	}()
	for _, block := range raw {
		copyBlock := clonePromptBlock(block)
		kind, _ := copyBlock["type"].(string)
		if kind != "file" && kind != "input_file" {
			result = append(result, copyBlock)
			continue
		}
		file, _ := copyBlock["file"].(map[string]any)
		if file == nil {
			return nil, fmt.Errorf("PDF 附件格式無效")
		}
		mediaType := strings.TrimSpace(stringValue(file["media_type"]))
		name := safePDFName(stringValue(file["name"]))
		if mediaType != "application/pdf" && !strings.EqualFold(filepath.Ext(name), ".pdf") {
			if _, err := attachments.DocumentType(name, mediaType); err == nil {
				result = append(result, copyBlock)
				continue
			}
			return nil, fmt.Errorf("目前檔案附件只支援 PDF")
		}
		path := strings.TrimSpace(stringValue(file["file_path"]))
		dataURL := strings.TrimSpace(stringValue(file["data_url"]))
		if (path == "") == (dataURL == "") {
			return nil, fmt.Errorf("PDF 附件必須提供一個檔案來源")
		}
		if dataURL != "" {
			var err error
			path, err = materializePDFDataURL(dataURL)
			if err != nil {
				return nil, err
			}
		} else {
			var err error
			path, err = materializePDFPath(path)
			if err != nil {
				return nil, err
			}
		}
		copyBlock["type"] = "file"
		copyBlock["file"] = map[string]any{
			"name": name, "media_type": "application/pdf", "file_path": path,
		}
		owned = append(owned, copyBlock)
		result = append(result, copyBlock)
	}
	return result, nil
}

func clonePromptBlock(block map[string]any) map[string]any {
	clone := make(map[string]any, len(block))
	for key, value := range block {
		clone[key] = value
	}
	if file, ok := block["file"].(map[string]any); ok {
		fileClone := make(map[string]any, len(file))
		for key, value := range file {
			fileClone[key] = value
		}
		clone["file"] = fileClone
	}
	return clone
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func safePDFName(raw string) string {
	name := filepath.Base(strings.TrimSpace(raw))
	name = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "." || name == "" {
		return "附件.pdf"
	}
	return name
}

func materializePDFDataURL(raw string) (string, error) {
	comma := strings.IndexByte(raw, ',')
	if comma <= 0 || !strings.EqualFold(raw[:comma], "data:application/pdf;base64") {
		return "", fmt.Errorf("PDF 附件必須是 base64 data URL")
	}
	encoded := raw[comma+1:]
	if len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(promptPDFMaxBytes)+4 {
		return "", fmt.Errorf("PDF 附件必須小於 64 MB")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("PDF 附件解碼失敗")
	}
	if len(data) < 5 || len(data) > promptPDFMaxBytes || string(data[:5]) != "%PDF-" {
		return "", fmt.Errorf("PDF 附件內容無效或超過 64 MB")
	}
	return writePromptPDF(data)
}

// materializePDFPath copies a Desktop-authorized drop into a new Core-owned
// directory. Rendering and queue persistence only see this immutable snapshot,
// so replacing the Desktop temp path after validation cannot change what the
// model receives.
func materializePDFPath(path string) (string, error) {
	data, err := readAuthorizedPromptPDF(path)
	if err != nil {
		return "", err
	}
	return writePromptPDF(data)
}

func writePromptPDF(data []byte) (string, error) {
	if len(data) < 5 || len(data) > promptPDFMaxBytes || string(data[:5]) != "%PDF-" {
		return "", fmt.Errorf("PDF 附件內容無效或超過 64 MB")
	}
	dir, err := os.MkdirTemp("", "meadow-pdf-attachment-")
	if err != nil {
		return "", fmt.Errorf("無法準備 PDF 附件")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("無法保護 PDF 附件")
	}
	path := filepath.Join(dir, "source.pdf")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("無法儲存 PDF 附件")
	}
	return path, nil
}

// readAuthorizedPromptPDF accepts exactly the private temp shape produced by
// Desktop. os.Root pins both the parent directory and source file while they
// are inspected and read, preventing symlink escape and path-swap TOCTOU.
func readAuthorizedPromptPDF(path string) ([]byte, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	tempRoot := filepath.Clean(os.TempDir())
	parentPath := filepath.Dir(path)
	if filepath.Dir(parentPath) != tempRoot || filepath.Base(path) != "source.pdf" {
		return nil, fmt.Errorf("PDF 附件路徑未經 Desktop 授權")
	}
	parent := filepath.Base(parentPath)
	if !strings.HasPrefix(parent, "meadow-pdf-drop-") &&
		!strings.HasPrefix(parent, "meadow-pdf-attachment-") {
		return nil, fmt.Errorf("PDF 附件路徑未經 Desktop 授權")
	}
	temp, err := os.OpenRoot(tempRoot)
	if err != nil {
		return nil, fmt.Errorf("PDF 附件無法讀取")
	}
	defer temp.Close()
	parentInfo, err := temp.Lstat(parent)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return nil, fmt.Errorf("PDF 附件路徑未經 Desktop 授權")
	}
	root, err := temp.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("PDF 附件無法讀取")
	}
	defer root.Close()
	openedParentInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(parentInfo, openedParentInfo) {
		return nil, fmt.Errorf("PDF 附件在讀取前已變更")
	}
	info, err := root.Lstat("source.pdf")
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > promptPDFMaxBytes {
		return nil, fmt.Errorf("PDF 附件無法讀取或超過 64 MB")
	}
	file, err := root.Open("source.pdf")
	if err != nil {
		return nil, fmt.Errorf("PDF 附件無法讀取")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("PDF 附件在讀取前已變更")
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(file, header); err != nil || string(header) != "%PDF-" {
		return nil, fmt.Errorf("PDF 附件內容無效")
	}
	rest, err := io.ReadAll(io.LimitReader(file, promptPDFMaxBytes-4))
	if err != nil || len(rest)+len(header) > promptPDFMaxBytes {
		return nil, fmt.Errorf("PDF 附件無法讀取或超過 64 MB")
	}
	return append(header, rest...), nil
}

func promptPDFBlock(block map[string]any) (name, path string, ok bool) {
	kind, _ := block["type"].(string)
	if kind != "file" && kind != "input_file" {
		return "", "", false
	}
	file, _ := block["file"].(map[string]any)
	if file == nil || strings.TrimSpace(stringValue(file["media_type"])) != "application/pdf" {
		return "", "", false
	}
	name = safePDFName(stringValue(file["name"]))
	path = strings.TrimSpace(stringValue(file["file_path"]))
	return name, path, name != "" && path != ""
}
