package browserworker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var errPolicyDenied = errors.New("browser worker policy denied request")

// NavigationPolicy is the complete network surface exposed to a browser
// backend. There is no wildcard, command, environment, or executable escape.
type NavigationPolicy interface {
	AuthorizeURL(rawURL string) error
}

type workspacePolicy struct {
	canonicalRoot string
	allowedOrigins map[string]struct{}
}

func newWorkspacePolicy(root string, origins []string) (*workspacePolicy, error) {
	if root == "" || len(root) > MaxPathBytes || strings.IndexByte(root, 0) >= 0 {
		return nil, errPolicyDenied
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, errPolicyDenied
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, errPolicyDenied
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return nil, errPolicyDenied
	}
	if len(origins) > MaxAllowedOrigins {
		return nil, errPolicyDenied
	}
	allowed := make(map[string]struct{}, len(origins))
	for _, rawOrigin := range origins {
		origin, err := canonicalOrigin(rawOrigin)
		if err != nil {
			return nil, errPolicyDenied
		}
		allowed[origin] = struct{}{}
	}
	return &workspacePolicy{
		canonicalRoot: filepath.Clean(canonical),
		allowedOrigins: allowed,
	}, nil
}

func (policy *workspacePolicy) resolveLocalArtifact(relativePath string) (LocalArtifact, error) {
	if relativePath == "" || len(relativePath) > MaxPathBytes || strings.IndexByte(relativePath, 0) >= 0 {
		return LocalArtifact{}, errPolicyDenied
	}
	if filepath.IsAbs(relativePath) || strings.Contains(relativePath, "://") {
		return LocalArtifact{}, errPolicyDenied
	}
	clean := filepath.Clean(relativePath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return LocalArtifact{}, errPolicyDenied
	}
	canonical, err := policy.canonicalFile(filepath.Join(policy.canonicalRoot, clean))
	if err != nil {
		return LocalArtifact{}, err
	}
	switch strings.ToLower(filepath.Ext(canonical)) {
	case ".html", ".htm", ".xhtml", ".svg", ".pdf":
	default:
		return LocalArtifact{}, errPolicyDenied
	}
	relativeCanonical, err := filepath.Rel(policy.canonicalRoot, canonical)
	if err != nil {
		return LocalArtifact{}, errPolicyDenied
	}
	digest := sha256.Sum256([]byte(filepath.ToSlash(relativeCanonical)))
	return LocalArtifact{
		canonicalPath: canonical,
		artifactID:    "artifact-" + hex.EncodeToString(digest[:8]),
	}, nil
}

func (policy *workspacePolicy) AuthorizeURL(rawURL string) error {
	if rawURL == "" || len(rawURL) > MaxURLBytes || strings.IndexByte(rawURL, 0) >= 0 {
		return errPolicyDenied
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil {
		return errPolicyDenied
	}
	switch strings.ToLower(parsed.Scheme) {
	case "about":
		if parsed.String() == "about:blank" {
			return nil
		}
	case "file":
		if parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errPolicyDenied
		}
		path, err := url.PathUnescape(parsed.EscapedPath())
		if err != nil {
			return errPolicyDenied
		}
		_, err = policy.canonicalFile(filepath.FromSlash(path))
		return err
	case "http", "https":
		origin, err := canonicalParsedOrigin(parsed)
		if err != nil {
			return errPolicyDenied
		}
		if _, ok := policy.allowedOrigins[origin]; ok {
			return nil
		}
	}
	return errPolicyDenied
}

func (policy *workspacePolicy) canonicalFile(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errPolicyDenied
	}
	canonical = filepath.Clean(canonical)
	relative, err := filepath.Rel(policy.canonicalRoot, canonical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errPolicyDenied
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		return "", errPolicyDenied
	}
	return canonical, nil
}

func canonicalOrigin(rawOrigin string) (string, error) {
	if rawOrigin == "" || len(rawOrigin) > MaxURLBytes || strings.IndexByte(rawOrigin, 0) >= 0 {
		return "", errPolicyDenied
	}
	parsed, err := url.Parse(rawOrigin)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errPolicyDenied
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errPolicyDenied
	}
	return canonicalParsedOrigin(parsed)
}

func canonicalParsedOrigin(parsed *url.URL) (string, error) {
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errPolicyDenied
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname == "" || strings.ContainsAny(hostname, " \t\r\n") {
		return "", errPolicyDenied
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return "", errPolicyDenied
		}
		host = net.JoinHostPort(hostname, port)
	}
	return fmt.Sprintf("%s://%s", scheme, host), nil
}

