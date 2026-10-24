package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	browseTimeout  = 45 * time.Second
	browseMaxBytes = 100_000
)

// NOTE: --virtual-time-budget is intentionally NOT used. With --headless=new it
// makes Chrome hang instead of exiting after --dump-dom/--screenshot (verified).
// --dump-dom already waits for the load event, so synchronous + on-load JS is
// captured; the context timeout below is the hard backstop.

// BrowsePage is the browse_page tool: render a URL in a headless Chromium-based
// browser (running its JavaScript) and return the rendered page text. web_fetch
// only sees the raw server HTML, so it misses content that JS injects —
// browse_page sees the page after scripts run, which is what dynamic / SPA
// comparison sites need. It shells out to a system Chrome / Chromium / Brave /
// Edge in headless mode (no node/Playwright dependency).
type BrowsePage struct {
	WS *Workspace
	// ChromePath overrides browser auto-discovery (mainly for tests).
	ChromePath string
}

func (t *BrowsePage) Name() string { return "browse_page" }
func (t *BrowsePage) Description() string {
	return "Open a URL in a headless browser (runs JavaScript) and return the rendered page text. Use for dynamic / JS-heavy pages that web_fetch cannot read."
}
func (t *BrowsePage) Kind() Kind                       { return KindExecute }
func (t *BrowsePage) ParallelSafe(map[string]any) bool { return true }
func (t *BrowsePage) SubtaskSafe() bool                { return true }

func (t *BrowsePage) EvidenceSources(args map[string]any, result Result) []EvidenceSource {
	rawURL := strings.TrimSpace(strArg(args, "url"))
	for _, line := range strings.Split(result.LLMContent, "\n") {
		if strings.HasPrefix(line, "Rendered text from ") {
			rawURL = strings.TrimSuffix(strings.TrimPrefix(line, "Rendered text from "), ":")
			break
		}
	}
	if source, ok := evidenceSourceForURL(rawURL, ""); ok {
		return []EvidenceSource{source}
	}
	return nil
}

func (t *BrowsePage) Schema() map[string]any {
	return obj(map[string]any{
		"url": str("Full http(s) URL to open."),
	}, "url")
}

func (t *BrowsePage) chrome() string {
	if t.ChromePath != "" {
		return t.ChromePath
	}
	return findChromeBinary()
}

func (t *BrowsePage) Execute(ctx context.Context, args map[string]any) Result {
	url := strings.TrimSpace(strArg(args, "url"))
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errResultf("url must start with http:// or https://")
	}
	chrome := t.chrome()
	if chrome == "" {
		return errResultf("no Chromium-based browser found (need Google Chrome / Chromium / Brave / Edge) — browse_page cannot run")
	}

	cctx, cancel := context.WithTimeout(ctx, browseTimeout)
	defer cancel()

	profile, err := os.MkdirTemp("", "meadow-browse-*")
	if err != nil {
		return errResult(err)
	}
	defer os.RemoveAll(profile)
	common := []string{
		"--headless=new", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--disable-extensions",
		// --no-proxy-server: macOS Chrome reads the *system* proxy config (not
		// env vars), and its proxy/PAC resolution hangs on loopback URLs (and
		// can stall others) — forcing a direct connection makes headless exit
		// reliably. Device-side users rarely sit behind a required proxy; if so
		// this is the knob to revisit.
		"--no-proxy-server",
		"--user-data-dir=" + profile,
	}

	// Dump the post-JS DOM, convert to readable text. (Screenshot mode is a
	// known follow-up: in --headless=new the PNG is written but Chrome does not
	// exit, so it needs a poll-the-file-then-kill path rather than wait-for-exit.)
	a := append(append([]string{}, common...), "--dump-dom", url)
	dom, err := runChromeCapture(cctx, chrome, a)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return errResultf("browse %s: timed out after %s", url, browseTimeout)
		}
		return errResultf("browse %s: %v", url, err)
	}
	text := htmlToText(dom)
	truncated := false
	if len(text) > browseMaxBytes {
		text = text[:browseMaxBytes]
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Rendered text from %s:\n\n%s", url, strings.TrimSpace(text))
	if truncated {
		b.WriteString("\n\n[content truncated at 100KB]")
	}
	return Result{LLMContent: b.String(), Display: "browsed " + url}
}

// findChromeBinary returns the path of the first Chromium-based browser found,
// or "" if none is installed.
func findChromeBinary() string {
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	} {
		if isExecFile(p) {
			return p
		}
	}
	for _, b := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave"} {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	return ""
}

func isExecFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// runChromeCapture runs Chrome writing stdout to a temp FILE, not an os/exec
// pipe. Chrome forks helper processes that inherit the stdout fd; an exec pipe
// only reaches EOF once ALL of them close it, so Output()/CombinedOutput() hang
// even after the main process has dumped the DOM and exited. A file fd sidesteps
// that — Wait() returns as soon as the main process exits.
func runChromeCapture(ctx context.Context, chrome string, args []string) (string, error) {
	f, err := os.CreateTemp("", "meadow-dom-*.html")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := exec.CommandContext(ctx, chrome, args...)
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		return "", err
	}
	b, err := os.ReadFile(f.Name())
	return string(b), err
}
