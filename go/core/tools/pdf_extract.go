package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	pdfExtractDefaultMaxChars = 24_000
	pdfExtractHardMaxChars    = 80_000
	pdfExtractMaxBytes        = 64 << 20
)

// PDFExtract gives the text-only Qwen office profile a bounded way to inspect
// local and web PDFs. It deliberately delegates parsing to pdftotext instead
// of asking the model to install a Python package or inject binary PDF bytes
// into its context.
type PDFExtract struct {
	WS       *Workspace
	HTTP     *http.Client
	Binary   string
	TempRoot string

	// Test-only seams remain package-private so a model or product config cannot
	// disable the outbound public-network boundary.
	allowPrivateTestTargets bool
	networkPolicy           *publicHTTPPolicy
}

func (t *PDFExtract) Name() string { return "pdf_extract" }
func (t *PDFExtract) Description() string {
	return "Extract bounded UTF-8 text from a local PDF or an http(s) PDF URL, optionally limited to a page range. Use this instead of shell downloads, pip installs, or read_file for PDFs."
}
func (t *PDFExtract) Kind() Kind                       { return KindExecute }
func (t *PDFExtract) ParallelSafe(map[string]any) bool { return true }
func (t *PDFExtract) SubtaskSafe() bool                { return true }
func (t *PDFExtract) EvidenceSources(args map[string]any, _ Result) []EvidenceSource {
	if source, ok := evidenceSourceForURL(strArg(args, "url"), ""); ok {
		return []EvidenceSource{source}
	}
	return nil
}
func (t *PDFExtract) NormalizeArguments(args map[string]any) {
	if strings.TrimSpace(strArg(args, "url")) != "" {
		return
	}
	filePath := strings.TrimSpace(strArg(args, "file_path"))
	parsed, err := url.Parse(filePath)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return
	}
	args["url"] = filePath
	delete(args, "file_path")
}

func (t *PDFExtract) Schema() map[string]any {
	return obj(map[string]any{
		"file_path":  str("Absolute local PDF path. Supply exactly one of file_path or url."),
		"url":        str("Full http(s) PDF URL. Supply exactly one of url or file_path."),
		"first_page": num("Optional first page, 1-based. Default 1."),
		"last_page":  num("Optional inclusive last page. Omit to read through the end."),
		"max_chars":  num("Maximum returned text characters from 2000 to 80000. Default 24000."),
	})
}

func (t *PDFExtract) client() *http.Client {
	var base *http.Client
	if t.HTTP != nil {
		base = t.HTTP
	} else {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	return guardPublicHTTPClient(base, t.allowPrivateTestTargets, t.networkPolicy)
}

func (t *PDFExtract) Execute(ctx context.Context, args map[string]any) Result {
	localArg := strings.TrimSpace(strArg(args, "file_path"))
	urlArg := strings.TrimSpace(strArg(args, "url"))
	if (localArg == "") == (urlArg == "") {
		return errResultf("supply exactly one of file_path or url")
	}

	firstPage := 1
	if value, ok := intArg(args, "first_page"); ok {
		if value < 1 || value > 100_000 {
			return errResultf("first_page must be between 1 and 100000")
		}
		firstPage = value
	}
	lastPage := 0
	if value, ok := intArg(args, "last_page"); ok {
		if value < firstPage || value > 100_000 {
			return errResultf("last_page must be between first_page and 100000")
		}
		lastPage = value
	}
	maxChars := pdfExtractDefaultMaxChars
	if value, ok := intArg(args, "max_chars"); ok {
		if value < 2_000 || value > pdfExtractHardMaxChars {
			return errResultf("max_chars must be between 2000 and %d", pdfExtractHardMaxChars)
		}
		maxChars = value
	}

	path := ""
	source := ""
	cleanup := func() {}
	if localArg != "" {
		resolved, err := t.WS.Resolve(localArg)
		if err != nil {
			return errResult(err)
		}
		if err := validatePDFFile(resolved); err != nil {
			return errResult(err)
		}
		path = resolved
		source = t.WS.ModelPath(resolved)
		t.WS.NoteRead(resolved)
	} else {
		parsed, err := url.Parse(urlArg)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errResultf("url must be a full http(s) URL")
		}
		var downloadCleanup func()
		path, downloadCleanup, err = t.download(ctx, urlArg)
		if err != nil {
			return errResult(err)
		}
		cleanup = downloadCleanup
		source = urlArg
	}
	defer cleanup()

	binary, err := t.extractor()
	if err != nil {
		return errResult(err)
	}
	commandArgs := []string{"-layout", "-enc", "UTF-8", "-f", fmt.Sprint(firstPage)}
	if lastPage > 0 {
		commandArgs = append(commandArgs, "-l", fmt.Sprint(lastPage))
	}
	commandArgs = append(commandArgs, path, "-")

	var stdout boundedPDFBuffer
	stdout.limit = pdfExtractHardMaxChars*4 + 4096
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, binary, commandArgs...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return errResultf("extract PDF %s: %v: %s", source, err, compactOneLine(detail, 240))
		}
		return errResultf("extract PDF %s: %v", source, err)
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return errResultf("PDF %s contains no extractable text; it may be scanned and require OCR", source)
	}
	runes := []rune(text)
	truncated := len(runes) > maxChars
	if truncated {
		text = string(runes[:maxChars])
	}
	pageRange := fmt.Sprintf("%d-end", firstPage)
	if lastPage > 0 {
		pageRange = fmt.Sprintf("%d-%d", firstPage, lastPage)
	}
	var result strings.Builder
	fmt.Fprintf(&result, "PDF text evidence\nsource: %s\npages: %s\ntext:\n%s", source, pageRange, text)
	if truncated || stdout.truncated {
		fmt.Fprintf(&result, "\n[PDF text truncated at %d characters; request a narrower page range for more evidence]", maxChars)
	}
	return Result{
		LLMContent: result.String(),
		Display:    fmt.Sprintf("PDF extracted: %s (pages %s)", compactOneLine(source, 72), pageRange),
	}
}

func (t *PDFExtract) download(ctx context.Context, rawURL string) (string, func(), error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", func() {}, err
	}
	request.Header.Set("Accept", "application/pdf")
	request.Header.Set("User-Agent", "MeadowCode/1.0")
	response, err := t.client().Do(request)
	if err != nil {
		return "", func() {}, fmt.Errorf("download PDF %s: %w", rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", func() {}, fmt.Errorf("download PDF %s: HTTP %d", rawURL, response.StatusCode)
	}
	if response.ContentLength > pdfExtractMaxBytes {
		return "", func() {}, fmt.Errorf("download PDF %s: file exceeds %d MiB", rawURL, pdfExtractMaxBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, pdfExtractMaxBytes+1))
	if err != nil {
		return "", func() {}, fmt.Errorf("download PDF %s: %w", rawURL, err)
	}
	if len(data) > pdfExtractMaxBytes {
		return "", func() {}, fmt.Errorf("download PDF %s: file exceeds %d MiB", rawURL, pdfExtractMaxBytes>>20)
	}
	if !looksLikePDF(data) {
		return "", func() {}, fmt.Errorf("download PDF %s: response is not a PDF", rawURL)
	}
	root := t.TempRoot
	dir, err := os.MkdirTemp(root, "meadow-pdf-")
	if err != nil {
		return "", func() {}, fmt.Errorf("prepare PDF extraction: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "source.pdf")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("prepare PDF extraction: %w", err)
	}
	return path, cleanup, nil
}

func (t *PDFExtract) extractor() (string, error) {
	if strings.TrimSpace(t.Binary) != "" {
		return t.Binary, nil
	}
	if path, err := exec.LookPath("pdftotext"); err == nil {
		return path, nil
	}
	for _, path := range []string{"/opt/homebrew/bin/pdftotext", "/usr/local/bin/pdftotext"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("pdftotext is unavailable; install Poppler or bundle pdftotext with Meadow")
}

func validatePDFFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > pdfExtractMaxBytes {
		return fmt.Errorf("%s exceeds %d MiB", path, pdfExtractMaxBytes>>20)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 5)
	if _, err := io.ReadFull(file, header); err != nil || !looksLikePDF(header) {
		return fmt.Errorf("%s is not a PDF", path)
	}
	return nil
}

func looksLikePDF(data []byte) bool {
	return len(data) >= 5 && string(data[:5]) == "%PDF-"
}

type boundedPDFBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedPDFBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			b.truncated = true
		}
		_, _ = b.Buffer.Write(value)
	} else {
		b.truncated = true
	}
	return original, nil
}
