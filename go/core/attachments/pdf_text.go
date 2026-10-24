package attachments

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	pdfTextMaxBytes       = 64 << 20 // The existing visual PDF upload limit.
	pdfTextTimeout        = 30 * time.Second
	pdfTextMaxOutputBytes = DocumentMaxTextChars*utf8.UTFMax + utf8.UTFMax
)

var (
	// ErrPDFTextUnavailable and ErrPDFTextEmpty let callers preserve visual PDF
	// processing while explaining why no searchable text was indexed.
	ErrPDFTextUnavailable = errors.New("pdftotext is unavailable")
	ErrPDFTextEmpty       = errors.New("PDF contains no extractable text; it may be scanned and require OCR")
)

// ExtractPDFText reads an already authorized, immutable local PDF. It retains
// pdftotext's UTF-8 layout text and form-feed page separators, never performs
// OCR, and never writes to the source file. Units counts pages represented in
// the retained extraction, not the original PDF's total page count.
func ExtractPDFText(ctx context.Context, path string) (DocumentExtract, error) {
	return extractPDFText(ctx, path, findPDFTextBinary)
}

// The resolver is private so callers cannot replace the extractor through
// attachment metadata. Tests supply a hermetic executable without changing PATH.
func extractPDFText(ctx context.Context, path string, resolveBinary func() (string, error)) (DocumentExtract, error) {
	result := DocumentExtract{
		LimitChars: DocumentMaxTextChars,
		Note:       "PDF UTF-8 文字，依 pdftotext -layout 順序保留換頁符；頁數只計保留文字所涵蓋的頁。未執行 OCR；圖片、圖表與視覺版面請參照原始 PDF。",
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("extract PDF text: %w", err)
	}
	if strings.TrimSpace(path) == "" {
		return result, fmt.Errorf("extract PDF text: local path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return result, fmt.Errorf("extract PDF text: resolve local path: %w", err)
	}
	if err := validatePDFTextFile(path); err != nil {
		return result, err
	}
	binary, err := resolveBinary()
	if err != nil {
		return result, err
	}

	extractCtx, cancel := context.WithTimeout(ctx, pdfTextTimeout)
	defer cancel()
	command := exec.CommandContext(extractCtx, binary, "-layout", "-enc", "UTF-8", "-f", "1", path, "-")
	command.WaitDelay = 250 * time.Millisecond
	stdout := &pdfTextBuffer{limit: pdfTextMaxOutputBytes}
	stderr := &pdfTextBuffer{limit: 4096}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if contextErr := extractCtx.Err(); contextErr != nil {
			return result, fmt.Errorf("extract PDF text: %w", contextErr)
		}
		detail := []rune(strings.Join(strings.Fields(strings.ToValidUTF8(stderr.String(), "�")), " "))
		if len(detail) > 240 {
			detail = detail[:240]
		}
		if len(detail) > 0 {
			return result, fmt.Errorf("extract PDF text: %w: %s", err, string(detail))
		}
		return result, fmt.Errorf("extract PDF text: %w", err)
	}
	if err := extractCtx.Err(); err != nil {
		return result, fmt.Errorf("extract PDF text: %w", err)
	}

	data := stdout.Bytes()
	// A byte cap can cut one UTF-8 code point. Only remove an incomplete final
	// code point when output was actually bounded; malformed output is an error.
	if stdout.truncated && !utf8.Valid(data) {
		for cut := 1; cut < utf8.UTFMax && cut <= len(data); cut++ {
			if !utf8.FullRune(data[len(data)-cut:]) && utf8.Valid(data[:len(data)-cut]) {
				data = data[:len(data)-cut]
				break
			}
		}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return result, fmt.Errorf("extract PDF text: extractor returned invalid UTF-8 text")
	}
	if strings.TrimSpace(string(data)) == "" {
		return result, ErrPDFTextEmpty
	}
	text := string(data)
	if utf8.RuneCountInString(text) > DocumentMaxTextChars {
		text = string([]rune(text)[:DocumentMaxTextChars])
		result.Truncated = true
	}
	result.Text = text
	result.Truncated = result.Truncated || stdout.truncated
	result.Units = retainedPDFPages(text)
	if result.Truncated {
		result.Note += " 文字已截斷，不能據此判定原始 PDF 的總頁數或全部內容。"
	}
	if err := extractCtx.Err(); err != nil {
		result.Text, result.Units, result.Truncated = "", 0, false
		return result, fmt.Errorf("extract PDF text: %w", err)
	}
	return result, nil
}

func findPDFTextBinary() (string, error) {
	if path, err := exec.LookPath("pdftotext"); err == nil {
		return path, nil
	}
	for _, path := range []string{"/opt/homebrew/bin/pdftotext", "/usr/local/bin/pdftotext"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", ErrPDFTextUnavailable
}

func validatePDFTextFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("extract PDF text: inspect source: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 5 || info.Size() > pdfTextMaxBytes {
		return fmt.Errorf("extract PDF text: source must be a regular PDF no larger than 64 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("extract PDF text: open source: %w", err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return fmt.Errorf("extract PDF text: inspect source: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 5 || info.Size() > pdfTextMaxBytes {
		return fmt.Errorf("extract PDF text: source must be a regular PDF no larger than 64 MiB")
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(file, header); err != nil || string(header) != "%PDF-" {
		return fmt.Errorf("extract PDF text: source is not a PDF")
	}
	return nil
}

func retainedPDFPages(text string) int {
	pages := strings.Count(text, "\f") + 1
	if lastBreak := strings.LastIndexByte(text, '\f'); lastBreak >= 0 && strings.TrimSpace(text[lastBreak+1:]) == "" {
		pages--
	}
	return pages
}

type pdfTextBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *pdfTextBuffer) Write(value []byte) (int, error) {
	length := len(value)
	remaining := max(0, b.limit-b.Len())
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(value)
	return length, nil
}
