package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	imageDownloadTimeout  = 30 * time.Second
	imageDownloadMaxBytes = 25 << 20 // 25 MiB
)

// DownloadImage is the download_image tool. web_fetch decodes the body as a
// UTF-8 string and returns it to the model (mangling binary, no file write),
// so it cannot save an image. This tool GETs a URL, keeps the raw bytes, and
// writes them to a workspace file — the missing piece for the
// "research → compare → download the product image" flow.
type DownloadImage struct {
	HTTP *http.Client
	WS   *Workspace

	// Test-only seams remain package-private so a model or product config cannot
	// disable the outbound public-network boundary.
	allowPrivateTestTargets bool
	networkPolicy           *publicHTTPPolicy
}

func (t *DownloadImage) Name() string { return "download_image" }
func (t *DownloadImage) Description() string {
	return "Download an image (or other binary file) from an http(s) URL and save it to a local file."
}
func (t *DownloadImage) Kind() Kind { return KindExecute } // network + write

func (t *DownloadImage) Schema() map[string]any {
	return obj(map[string]any{
		"url":       str("Full http(s) URL of the image to download (a direct image URL, not a web page)."),
		"file_path": str("Local path to save to; the extension should match the image type (e.g. .jpg, .png)."),
	}, "url", "file_path")
}

func (t *DownloadImage) client() *http.Client {
	var base *http.Client
	if t.HTTP != nil {
		base = t.HTTP
	} else {
		base = &http.Client{Timeout: imageDownloadTimeout}
	}
	return guardPublicHTTPClient(base, t.allowPrivateTestTargets, t.networkPolicy)
}

func (t *DownloadImage) Execute(ctx context.Context, args map[string]any) Result {
	url := strings.TrimSpace(strArg(args, "url"))
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errResultf("url must start with http:// or https://")
	}
	rawPath := pathArg(args)
	if rawPath == "" {
		return errResultf("file_path is required")
	}
	dest, err := t.WS.Resolve(rawPath)
	if err != nil {
		return errResult(err)
	}
	if err := t.WS.CheckWritable(dest); err != nil {
		return errResult(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errResult(err)
	}
	req.Header.Set("Accept", "image/*,*/*")
	req.Header.Set("User-Agent", "MeadowCode/1.0")

	resp, err := t.client().Do(req)
	if err != nil {
		return errResultf("download %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errResultf("download %s: HTTP %d", url, resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	// A text/html body means we hit a web page, not an image — saving it as
	// .png would silently produce a broken file, so reject with a hint.
	if strings.HasPrefix(ct, "text/html") {
		return errResultf("download %s: server returned HTML (Content-Type %q), not an image — use a direct image URL", url, ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, imageDownloadMaxBytes+1))
	if err != nil {
		return errResult(err)
	}
	if len(body) == 0 {
		return errResultf("download %s: empty response body", url)
	}
	if len(body) > imageDownloadMaxBytes {
		return errResultf("download %s: file exceeds %d MiB limit", url, imageDownloadMaxBytes>>20)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return errResult(err)
	}
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		return errResult(err)
	}
	t.WS.NoteRead(dest)

	msg := fmt.Sprintf("Downloaded %s → %s (%d bytes, %s)", url, dest, len(body), contentTypeOrUnknown(ct))
	return Result{LLMContent: msg, Display: fmt.Sprintf("downloaded %s (%d KB)", filepath.Base(dest), len(body)/1024)}
}

func contentTypeOrUnknown(ct string) string {
	if ct == "" {
		return "unknown type"
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct
}
