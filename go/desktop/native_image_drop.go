package main

import (
	"context"

	"encoding/base64"
	"fmt"
	"github.com/hey-meadow/meadow-code/go/core/buildinfo"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/attachments"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const (
	maxNativeDroppedImages   = 4
	maxNativeDroppedImageLen = 16 << 20
	maxNativeDroppedPDFLen   = 64 << 20
)

type nativeDroppedImage struct {
	Name string `json:"name"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

type nativeDroppedPDF struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path"`
}

type nativeDroppedDocument struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path"`
}

func readNativeDroppedDocument(path string) (nativeDroppedDocument, error) {
	path = strings.TrimSpace(path)
	format, err := attachments.DocumentType(filepath.Base(path), "")
	if err != nil {
		return nativeDroppedDocument{}, err
	}
	source, err := os.Open(path)
	if err != nil {
		return nativeDroppedDocument{}, fmt.Errorf("無法讀取 %s", filepath.Base(path))
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > attachments.DocumentMaxBytes {
		return nativeDroppedDocument{}, fmt.Errorf("文件必須是有效檔案且不超過 20 MB")
	}
	data, err := io.ReadAll(io.LimitReader(source, attachments.DocumentMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > attachments.DocumentMaxBytes {
		return nativeDroppedDocument{}, fmt.Errorf("文件無法讀取或超過 20 MB")
	}
	if _, err := attachments.ExtractDocument(filepath.Base(path), format.MediaType, data); err != nil {
		return nativeDroppedDocument{}, err
	}
	dir, err := os.MkdirTemp("", "meadow-document-drop-")
	if err != nil {
		return nativeDroppedDocument{}, fmt.Errorf("無法準備文件附件")
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return nativeDroppedDocument{}, fmt.Errorf("無法保護文件附件")
	}
	destination := filepath.Join(dir, "source"+format.Extension)
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		cleanup()
		return nativeDroppedDocument{}, fmt.Errorf("無法複製文件附件")
	}
	return nativeDroppedDocument{Name: filepath.Base(path), Type: format.MediaType, Path: destination}, nil
}

func readNativeDroppedImage(path string) (nativeDroppedImage, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nativeDroppedImage{}, fmt.Errorf("拖放路徑是空的")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nativeDroppedImage{}, fmt.Errorf("無法讀取 %s", filepath.Base(path))
	}
	if !info.Mode().IsRegular() {
		return nativeDroppedImage{}, fmt.Errorf("%s 不是圖片檔案", filepath.Base(path))
	}
	if info.Size() > maxNativeDroppedImageLen {
		return nativeDroppedImage{}, fmt.Errorf("%s 超過 16 MB", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nativeDroppedImage{}, fmt.Errorf("無法讀取 %s", filepath.Base(path))
	}
	mimeType := http.DetectContentType(data)
	if !strings.HasPrefix(mimeType, "image/") {
		return nativeDroppedImage{}, fmt.Errorf("%s 不是支援的圖片", filepath.Base(path))
	}
	return nativeDroppedImage{
		Name: filepath.Base(path),
		Type: mimeType,
		URL:  "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
	}, nil
}

// readNativeDroppedPDF copies an explicitly dropped PDF into a private temp
// directory. WebKit file inputs do not expose host paths, and the agent's
// workspace boundary intentionally rejects arbitrary absolute files. The temp
// copy is both user-authorized and readable by PDFExtract without widening the
// model's filesystem access.
func readNativeDroppedPDF(path string) (nativeDroppedPDF, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nativeDroppedPDF{}, fmt.Errorf("拖放路徑是空的")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nativeDroppedPDF{}, fmt.Errorf("無法讀取 %s", filepath.Base(path))
	}
	if !info.Mode().IsRegular() {
		return nativeDroppedPDF{}, fmt.Errorf("%s 不是 PDF 檔案", filepath.Base(path))
	}
	if info.Size() <= 0 || info.Size() > maxNativeDroppedPDFLen {
		return nativeDroppedPDF{}, fmt.Errorf("%s 必須小於 64 MB", filepath.Base(path))
	}
	source, err := os.Open(path)
	if err != nil {
		return nativeDroppedPDF{}, fmt.Errorf("無法讀取 %s", filepath.Base(path))
	}
	defer source.Close()
	header := make([]byte, 5)
	if _, err := io.ReadFull(source, header); err != nil || string(header) != "%PDF-" {
		return nativeDroppedPDF{}, fmt.Errorf("%s 不是有效的 PDF", filepath.Base(path))
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nativeDroppedPDF{}, fmt.Errorf("無法讀取 %s", filepath.Base(path))
	}
	dir, err := os.MkdirTemp("", "meadow-pdf-drop-")
	if err != nil {
		return nativeDroppedPDF{}, fmt.Errorf("無法準備 PDF 附件")
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return nativeDroppedPDF{}, fmt.Errorf("無法保護 PDF 附件")
	}
	destinationPath := filepath.Join(dir, "source.pdf")
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return nativeDroppedPDF{}, fmt.Errorf("無法準備 PDF 附件")
	}
	_, copyErr := io.Copy(destination, io.LimitReader(source, maxNativeDroppedPDFLen+1))
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil {
		cleanup()
		return nativeDroppedPDF{}, fmt.Errorf("無法複製 PDF 附件")
	}
	return nativeDroppedPDF{
		Name: filepath.Base(path),
		Type: "application/pdf",
		Path: destinationPath,
	}, nil
}

// processNativeImageDrop keeps account admission ahead of even retrieving host
// paths. The injected callbacks let tests exercise the native boundary without
// starting Wails. No account error or dropped-file detail is emitted on denial.
func processNativeImageDrop(auth desktopProductAuth, droppedFiles func() []string, emit func(string, map[string]any)) {
	authorized := func() bool {
		if buildinfo.VMLab {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := desktopAccountStatus(ctx, auth)
		return err == nil && ctx.Err() == nil
	}
	if !authorized() {
		return
	}
	paths := droppedFiles()
	if len(paths) == 0 {
		return
	}
	dropID := session.NewUUID()
	images := make([]nativeDroppedImage, 0, min(len(paths), maxNativeDroppedImages))
	pdfs := make([]nativeDroppedPDF, 0, min(len(paths), maxNativeDroppedImages))
	documents := make([]nativeDroppedDocument, 0, min(len(paths), maxNativeDroppedImages))
	rejected := make([]string, 0)
	ownedTempDirs := make([]string, 0, maxNativeDroppedImages)
	delivered := false
	defer func() {
		if !delivered {
			// These directories come only from successful fresh snapshots made by
			// this callback, never from a dropped path or a global temp scan.
			for _, dir := range ownedTempDirs {
				_ = os.RemoveAll(dir)
			}
		}
	}()
	for index, path := range paths {
		if !authorized() {
			return
		}
		if index == 0 {
			// Capture frontend thread ownership before the first file read.
			emit("meadow:files-drop-started", map[string]any{"drop_id": dropID})
		}
		if len(images)+len(pdfs)+len(documents) >= maxNativeDroppedImages {
			rejected = append(rejected, fmt.Sprintf("一次最多附 %d 個檔案", maxNativeDroppedImages))
			break
		}
		if strings.EqualFold(filepath.Ext(path), ".pdf") {
			pdf, err := readNativeDroppedPDF(path)
			if err != nil {
				rejected = append(rejected, err.Error())
				continue
			}
			ownedTempDirs = append(ownedTempDirs, filepath.Dir(pdf.Path))
			pdfs = append(pdfs, pdf)
			continue
		}
		if _, err := attachments.DocumentType(filepath.Base(path), ""); err == nil || strings.EqualFold(filepath.Ext(path), ".xls") {
			document, err := readNativeDroppedDocument(path)
			if err != nil {
				rejected = append(rejected, err.Error())
				continue
			}
			ownedTempDirs = append(ownedTempDirs, filepath.Dir(document.Path))
			documents = append(documents, document)
			continue
		}
		image, err := readNativeDroppedImage(path)
		if err != nil {
			rejected = append(rejected, err.Error())
			continue
		}
		images = append(images, image)
	}
	if !authorized() {
		return
	}
	emit("meadow:image-files-dropped", map[string]any{
		"drop_id": dropID, "images": images, "pdfs": pdfs, "documents": documents, "rejected": rejected,
	})
	delivered = true
}

func registerNativeImageDrop(window *application.WebviewWindow, auth desktopProductAuth) {
	window.OnWindowEvent(events.Common.WindowFilesDropped, func(event *application.WindowEvent) {
		processNativeImageDrop(auth, func() []string {
			return event.Context().DroppedFiles()
		}, func(name string, payload map[string]any) {
			application.Get().Event.Emit(name, payload)
		})
	})
}
