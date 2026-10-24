//go:build !darwin || !cgo

package serve

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
)

func renderPDFPages(source string, maxPages, maxLongEdge, quality int) ([]renderedPDFPage, error) {
	binary, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil, fmt.Errorf("此平台缺少 PDF 視覺渲染器 pdftoppm")
	}
	dir, err := os.MkdirTemp("", "meadow-pdf-render-")
	if err != nil {
		return nil, fmt.Errorf("無法建立 PDF 頁面暫存區")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("無法保護 PDF 頁面暫存區")
	}
	prefix := filepath.Join(dir, "page")
	command := exec.Command(binary,
		"-f", "1", "-l", strconv.Itoa(maxPages+1),
		"-scale-to", strconv.Itoa(maxLongEdge),
		"-jpeg", "-jpegopt", "quality="+strconv.Itoa(quality),
		source, prefix,
	)
	if output, err := command.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pdftoppm 失敗：%s", string(output))
	}
	paths, _ := filepath.Glob(prefix + "-*.jpg")
	sort.Strings(paths)
	if len(paths) > maxPages {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("PDF 超過 %d 頁，請拆成較小檔案", maxPages)
	}
	if len(paths) == 0 {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("PDF 沒有可渲染的頁面")
	}
	pages := make([]renderedPDFPage, 0, len(paths))
	for index, oldPath := range paths {
		newPath := filepath.Join(dir, fmt.Sprintf("page-%03d.jpg", index+1))
		if oldPath != newPath {
			if err := os.Rename(oldPath, newPath); err != nil {
				_ = os.RemoveAll(dir)
				return nil, fmt.Errorf("無法整理 PDF 頁面影像")
			}
		}
		_ = os.Chmod(newPath, 0o600)
		pages = append(pages, renderedPDFPage{
			Number: index + 1, Count: len(paths), MediaType: "image/jpeg", FilePath: newPath,
		})
	}
	return pages, nil
}
