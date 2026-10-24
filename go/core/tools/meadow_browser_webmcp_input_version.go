package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

var _ ExecutionInputVersionProvider = (*MeadowBrowserWebMCP)(nil)

// ToolExecutionInputVersion identifies only the original local entry document
// for discovery. It performs no Browser request, source repair, or snapshot
// creation; its identity never comes from model arguments or page declarations.
func (t *MeadowBrowserWebMCP) ToolExecutionInputVersion(ctx context.Context, args map[string]any) (string, error) {
	if strings.ToLower(strings.TrimSpace(strArg(args, "action"))) != "discover" {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := t.webMCPLocalDiscoverySource(args)
	if err != nil || path == "" {
		return "", err
	}
	digest, err := webMCPSourceSHA(ctx, t.WS, path)
	if err != nil {
		return "", fmt.Errorf("read WebMCP discovery input identity: %w", err)
	}
	return webMCPDiscoveryInputVersion(path, digest), nil
}

func webMCPDiscoveryInputVersion(path, digest string) string {
	identity := sha256.Sum256([]byte("meadow.webmcp.local-discover.v1\x00" + path + "\x00" + digest))
	return fmt.Sprintf("%x", identity)
}

// Resolve the same saved source used by discovery, without targetURL's legacy
// protocol-leak repair. URL hints may change a route/query but cannot select a
// different file or use Workspace.Resolve's basename aliases for a foreign URI.
func (t *MeadowBrowserWebMCP) webMCPLocalDiscoverySource(args map[string]any) (string, error) {
	raw := strings.TrimSpace(strArg(args, "file_path"))
	rawURL := strings.TrimSpace(strArg(args, "url"))
	if raw != "" {
		if strings.Contains(raw, "://") {
			return "", fmt.Errorf("file_path requires a saved local path, not a URL")
		}
		if t.WS == nil {
			return "", fmt.Errorf("workspace is not configured")
		}
		path, err := t.WS.Resolve(raw)
		if err != nil {
			return "", err
		}
		if rawURL != "" {
			hint, err := url.Parse(rawURL)
			if err != nil {
				return "", fmt.Errorf("invalid url alongside file_path: %w", err)
			}
			suffixOnly := hint.Scheme == "" && hint.Host == "" && hint.Path == "" && hint.RawPath == "" && hint.Opaque == "" && hint.User == nil
			hintPath := filepath.Clean(filepath.FromSlash(hint.Path))
			samePath := hintPath == filepath.Clean(path) || (filepath.IsAbs(raw) && hintPath == filepath.Clean(raw))
			matchingFile := webMCPFileURL(hint) && samePath
			if !suffixOnly && !matchingFile {
				return "", fmt.Errorf("url conflicts with file_path: supply only a query/fragment suffix or a file:// URL for the same saved path")
			}
		}
		return t.webMCPCanonicalSource(path)
	}
	if rawURL == "" {
		return "", nil // Execute reports the missing discovery target.
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "file" {
		return "", nil // Remote discovery has no local input version.
	}
	if !webMCPFileURL(parsed) || !webMCPPathInWorkspace(t.WS, filepath.FromSlash(parsed.Path)) {
		return "", fmt.Errorf("file URL must be inside the active workspace%s", webMCPVirtualFileHint(parsed))
	}
	return t.webMCPCanonicalSource(filepath.FromSlash(parsed.Path))
}

func (t *MeadowBrowserWebMCP) webMCPCanonicalSource(path string) (string, error) {
	canonical, err := canonicalPathAllowMissing(path)
	if err != nil {
		return "", err
	}
	if !webMCPPathInWorkspace(t.WS, canonical) {
		return "", fmt.Errorf("WebMCP discovery input must be inside the active workspace or session")
	}
	return canonical, nil
}

func (t *MeadowBrowserWebMCP) requireWebMCPExecutionInputVersion(ctx context.Context, args map[string]any) error {
	expected := ExpectedExecutionInputVersion(ctx)
	if expected == "" {
		return nil // Preserve direct callers and remote discovery.
	}
	actual, err := t.ToolExecutionInputVersion(ctx, args)
	if err != nil {
		return fmt.Errorf("WebMCP discovery input cannot be verified before navigation: %w; no navigation occurred; discover again from the saved artifact", err)
	}
	if actual == "" || actual != expected {
		return fmt.Errorf("WebMCP discovery input changed before navigation; no navigation occurred; discover again from the saved artifact")
	}
	return nil
}

// The original source can change A -> B -> A while snapshotting. Its pre/post
// digest alone cannot stamp the copied B bytes as A. Bind the actual retained
// entry bytes to the original canonical source identity instead.
func (t *MeadowBrowserWebMCP) requireWebMCPSnapshotInputVersion(ctx context.Context, sourcePath, sourceSHA, target string) error {
	parsed, err := url.Parse(target)
	if err != nil || !webMCPFileURL(parsed) {
		return fmt.Errorf("WebMCP snapshot must be a generated local file document")
	}
	snapshotPath, err := t.webMCPCanonicalSource(filepath.FromSlash(parsed.Path))
	if err != nil {
		return fmt.Errorf("verify WebMCP snapshot identity: %w", err)
	}
	snapshotSHA, err := webMCPSourceSHA(ctx, t.WS, snapshotPath)
	if err != nil {
		return fmt.Errorf("verify WebMCP snapshot bytes: %w", err)
	}
	if snapshotSHA != sourceSHA {
		return fmt.Errorf("WebMCP snapshot bytes differ from the saved discovery input; discover again from the saved artifact")
	}
	if expected := ExpectedExecutionInputVersion(ctx); expected != "" {
		canonical, err := t.webMCPCanonicalSource(sourcePath)
		if err != nil || webMCPDiscoveryInputVersion(canonical, snapshotSHA) != expected {
			return fmt.Errorf("WebMCP snapshot does not match the approved discovery input; discover again from the saved artifact")
		}
	}
	return nil
}
