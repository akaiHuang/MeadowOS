package tools

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A per-check loopback origin preserves WebKit exception diagnostics, unlike
// opaque file origins. It serves only this snapshot's files, never a listing or
// a write API. No origin-security preferences or application scripts are changed.
func serveBrowserSnapshot(snapshotPath, sourceRoot string) (string, func(), error) {
	root, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return "", func() {}, err
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return "", func() {}, err
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		rootFS.Close()
		return "", func() {}, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		rootFS.Close()
		return "", func() {}, err
	}
	host := listener.Addr().String()
	prefix := "/" + hex.EncodeToString(token[:]) + "/"
	directory := filepath.Dir(snapshotPath)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rel := strings.TrimPrefix(r.URL.Path, prefix)
		// Reject traversal, hidden metadata and platform-dependent separators.
		for _, part := range strings.Split(rel, "/") {
			if part == "" || strings.HasPrefix(part, ".") || strings.Contains(part, "\\") {
				http.NotFound(w, r)
				return
			}
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(directory, filepath.FromSlash(rel)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		within, err := filepath.Rel(root, resolved)
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
			http.NotFound(w, r)
			return
		}
		for i, part := range strings.Split(within, string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") && !(i == 0 && part == filepath.Base(directory)) {
				http.NotFound(w, r)
				return
			}
		}
		// Root.Open prevents a symlink swap between resolution and open from
		// escaping the authorized source tree.
		file, err := rootFS.Open(within)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	u := url.URL{Scheme: "http", Host: host, Path: prefix + filepath.Base(snapshotPath)}
	return u.String(), func() { _ = server.Close(); _ = rootFS.Close() }, nil
}
