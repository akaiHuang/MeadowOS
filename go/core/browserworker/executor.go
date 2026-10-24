package browserworker

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var ErrExecutorUnavailable = errors.New("browser executor unavailable")

var safeDOMToken = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Executor is intentionally narrower than a process or browser automation API.
// Implementations receive no executable, argv, environment, prompt, or shell
// fields and must enforce NavigationPolicy on every navigation and subresource.
type Executor interface {
	LoadLocalArtifact(context.Context, LocalArtifact, NavigationPolicy) (LoadResult, error)
	ConsoleRuntime(context.Context, ConsoleRuntimeRequest) (ConsoleRuntimeResult, error)
	DOMSummary(context.Context, DOMSummaryRequest) (DOMSummaryResult, error)
	ScreenshotMetadata(context.Context) (ScreenshotMetadataResult, error)
}

// ManagedExecutor is a platform browser executor whose UI event loop must own
// the process main thread. meadow-browserd serves the bounded Unix protocol on
// a worker goroutine while Run services WebKit on the locked Darwin main thread.
type ManagedExecutor interface {
	Executor
	Run(context.Context) error
	Close() error
}

type LocalArtifact struct {
	canonicalPath string
	artifactID    string
}

func (artifact LocalArtifact) CanonicalPath() string { return artifact.canonicalPath }
func (artifact LocalArtifact) ArtifactID() string    { return artifact.artifactID }

type LoadResult struct {
	Ready       bool   `json:"ready"`
	ContentType string `json:"content_type"`
}

type ConsoleRuntimeRequest struct {
	MaxEvents int `json:"max_events"`
}

type ConsoleRuntimeResult struct {
	Runtime RuntimeSnapshot `json:"runtime"`
	Events  []ConsoleEvent  `json:"events"`
	Truncated bool          `json:"truncated"`
}

type RuntimeSnapshot struct {
	State              string `json:"state"`
	JavaScriptContexts int    `json:"javascript_contexts"`
	PendingRequests    int    `json:"pending_requests"`
}

// ConsoleEvent deliberately omits message text, URLs, stack traces, source
// paths, arguments, and values. It exposes only bounded diagnostic metadata.
type ConsoleEvent struct {
	Sequence uint32 `json:"sequence"`
	Level    string `json:"level"`
	Category string `json:"category"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}

type DOMSummaryRequest struct {
	MaxNodes int `json:"max_nodes"`
	MaxDepth int `json:"max_depth"`
}

// DOMNode is structural only. Text, values, attributes, HTML, selectors, URLs,
// prompts, and accessibility names never cross the helper boundary.
type DOMNode struct {
	Index       int       `json:"index"`
	ParentIndex int       `json:"parent_index"`
	Depth       int       `json:"depth"`
	Tag         string    `json:"tag"`
	Role        string    `json:"role,omitempty"`
	Interactive bool      `json:"interactive"`
	Visible     bool      `json:"visible"`
	ChildCount  int       `json:"child_count"`
	Bounds      DOMBounds `json:"bounds"`
}

type DOMBounds struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type DOMSummaryResult struct {
	Nodes     []DOMNode `json:"nodes"`
	Truncated bool      `json:"truncated"`
}

// ScreenshotMetadataResult intentionally contains no bytes or filesystem path.
type ScreenshotMetadataResult struct {
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Format    string `json:"format"`
	ByteSize  int64  `json:"byte_size"`
	SHA256    string `json:"sha256"`
}

func validateLoadResult(result LoadResult) error {
	switch strings.ToLower(result.ContentType) {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "application/pdf":
		return nil
	default:
		return errors.New("invalid browser content type")
	}
}

func validateConsoleRuntimeResult(result ConsoleRuntimeResult, limit int) error {
	switch result.Runtime.State {
	case "idle", "loading", "ready", "failed", "closed":
	default:
		return errors.New("invalid runtime state")
	}
	if result.Runtime.JavaScriptContexts < 0 || result.Runtime.JavaScriptContexts > 10000 ||
		result.Runtime.PendingRequests < 0 || result.Runtime.PendingRequests > 100000 ||
		len(result.Events) > limit || len(result.Events) > MaxConsoleEvents {
		return errors.New("invalid runtime bounds")
	}
	var previous uint32
	for _, event := range result.Events {
		if event.Sequence == 0 || event.Sequence <= previous || event.Line < 0 || event.Line > 10000000 || event.Column < 0 || event.Column > 1000000 {
			return errors.New("invalid console event bounds")
		}
		previous = event.Sequence
		switch event.Level {
		case "debug", "info", "warning", "error":
		default:
			return errors.New("invalid console level")
		}
		switch event.Category {
		case "console", "runtime", "network", "security":
		default:
			return errors.New("invalid console category")
		}
	}
	return nil
}

func validateDOMSummaryResult(result DOMSummaryResult, request DOMSummaryRequest) error {
	if len(result.Nodes) > request.MaxNodes || len(result.Nodes) > MaxDOMNodes {
		return errors.New("invalid DOM node count")
	}
	for position, node := range result.Nodes {
		if node.Index != position || node.ParentIndex < -1 || node.ParentIndex >= position || node.Depth < 0 || node.Depth > request.MaxDepth ||
			node.ChildCount < 0 || node.ChildCount > MaxDOMNodes || !safeDOMToken.MatchString(strings.ToLower(node.Tag)) {
			return errors.New("invalid DOM node")
		}
		if node.Role != "" && !safeDOMToken.MatchString(strings.ToLower(node.Role)) {
			return errors.New("invalid DOM role")
		}
		if node.Bounds.X < -1000000 || node.Bounds.X > 1000000 || node.Bounds.Y < -1000000 || node.Bounds.Y > 1000000 ||
			node.Bounds.Width < 0 || node.Bounds.Width > 100000 || node.Bounds.Height < 0 || node.Bounds.Height > 100000 {
			return errors.New("invalid DOM bounds")
		}
	}
	return nil
}

func validateScreenshotMetadata(result ScreenshotMetadataResult) error {
	if result.Width <= 0 || result.Width > 32768 || result.Height <= 0 || result.Height > 32768 ||
		result.ByteSize <= 0 || result.ByteSize > 512<<20 || !sha256Pattern.MatchString(result.SHA256) {
		return errors.New("invalid screenshot metadata")
	}
	switch result.Format {
	case "png", "jpeg", "webp":
		return nil
	default:
		return errors.New("invalid screenshot format")
	}
}
