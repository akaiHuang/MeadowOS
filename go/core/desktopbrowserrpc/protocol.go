// Package desktopbrowserrpc carries bounded Browser HTTP contracts over an
// inherited, authenticated connection to the actual Desktop WKWebView. It
// never discovers, dials, launches, or falls back to a network Browser.
package desktopbrowserrpc

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	BaseURL          = "meadow-desktop-private://browser"
	HeaderSession    = "X-Meadow-Browser-Session"
	HeaderWorkspace  = "X-Meadow-Browser-Workspace"
	HeaderPage       = "X-Meadow-Browser-Page"
	MaxRequestBytes  = 1 << 20
	MaxResponseBytes = 64 << 20
	MaxActive        = 8
	PeerSecretBytes  = 32
	protocolVersion  = "meadow.desktop-browser.v1"
	maxMetadataBytes = 64 << 10
	maxRequestFrame  = (MaxRequestBytes*4+2)/3 + maxMetadataBytes
	maxResponseFrame = (MaxResponseBytes*4+2)/3 + maxMetadataBytes
	ioTimeout        = 5 * time.Second
)

var (
	ErrUnavailable    = errors.New("browser_environment_unavailable: Desktop embedded Browser channel unavailable")
	ErrAuthentication = errors.New("Desktop embedded Browser authentication failed")
	ErrLimit          = errors.New("Desktop embedded Browser transport limit exceeded")
	ErrInvalid        = errors.New("Desktop embedded Browser request rejected")
	ErrPageContext    = errors.New("browser_page_context_unknown: establish the Desktop Browser page with navigation or WebMCP discovery")
)

type ClientFactory func(sessionID, workspaceRoot string) *http.Client

type frame struct {
	Version       string      `json:"version"`
	Kind          string      `json:"kind"`
	ID            uint64      `json:"id,omitempty"`
	Nonce         string      `json:"nonce,omitempty"`
	Proof         string      `json:"proof,omitempty"`
	SessionID     string      `json:"session_id,omitempty"`
	WorkspaceRoot string      `json:"workspace_root,omitempty"`
	PageID        string      `json:"page_id,omitempty"`
	Method        string      `json:"method,omitempty"`
	Path          string      `json:"path,omitempty"`
	Status        int         `json:"status,omitempty"`
	Headers       http.Header `json:"headers,omitempty"`
	Body          []byte      `json:"body,omitempty"`
}

func readFrame(reader io.Reader, limit uint32) (frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return frame{}, err
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size == 0 || size > limit {
		return frame{}, ErrLimit
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(reader, data); err != nil {
		return frame{}, err
	}
	var value frame
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return frame{}, ErrInvalid
	}
	if decoder.Decode(new(any)) != io.EOF || value.Version != protocolVersion {
		return frame{}, ErrInvalid
	}
	return value, nil
}

type frameWriter struct {
	conn net.Conn
	gate chan struct{}
}

func newFrameWriter(conn net.Conn) *frameWriter {
	return &frameWriter{conn: conn, gate: make(chan struct{}, 1)}
}

func (writer *frameWriter) acquire(ctx context.Context) error {
	select {
	case writer.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			writer.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (writer *frameWriter) release() { <-writer.gate }

func (writer *frameWriter) write(ctx context.Context, value frame, limit int) error {
	if err := writer.acquire(ctx); err != nil {
		return err
	}
	defer writer.release()
	return writer.writeLocked(ctx, value, limit)
}

func (writer *frameWriter) writeLocked(ctx context.Context, value frame, limit int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value.Version = protocolVersion
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > limit {
		return ErrLimit
	}
	deadline := time.Now().Add(ioTimeout)
	if caller, ok := ctx.Deadline(); ok && caller.Before(deadline) {
		deadline = caller
	}
	if err := writer.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		_ = writer.conn.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		if !stopInterrupt() {
			<-interrupted
		}
		_ = writer.conn.SetWriteDeadline(time.Time{})
	}()
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))
	if err := writeAll(writer.conn, prefix[:]); err != nil {
		// A partial frame cannot be followed by another frame safely. Retire
		// the channel, so a cancelled mutation is never retried or replayed.
		_ = writer.conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := writeAll(writer.conn, data); err != nil {
		_ = writer.conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func authProof(secret []byte, side, nonce string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, protocolVersion+":"+side+":"+nonce)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validProof(secret []byte, side, nonce, proof string) bool {
	if len(nonce) != base64.RawURLEncoding.EncodedLen(PeerSecretBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(decoded) != PeerSecretBytes {
		return false
	}
	return hmac.Equal([]byte(authProof(secret, side, nonce)), []byte(proof))
}

func validOwner(session, workspace string) bool {
	if len(session) == 0 || len(session) > 128 || len(workspace) == 0 || len(workspace) > 4096 || !filepath.IsAbs(workspace) || strings.IndexByte(workspace, 0) >= 0 {
		return false
	}
	for _, character := range session {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
			return false
		}
	}
	return filepath.Clean(workspace) == workspace
}

func validRoute(method, path string) bool {
	if len(path) == 0 || len(path) > 4096 {
		return false
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" || parsed.EscapedPath() != parsed.Path {
		return false
	}
	if method == http.MethodGet {
		switch parsed.Path {
		case "/health", "/state", "/diagnostics", "/console", "/webmcp/tools":
			return true
		}
	}
	if method == http.MethodPost {
		switch parsed.Path {
		case "/goto", "/dom", "/eval", "/js", "/click", "/fill", "/press", "/screenshot", "/extract", "/webmcp/execute":
			return true
		}
	}
	return false
}

func validPrivateURL(value *url.URL) bool {
	return value != nil && value.Scheme == "meadow-desktop-private" && value.Host == "browser" && value.User == nil && value.Fragment == "" && value.Opaque == ""
}

func validPageID(value string) bool {
	return len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00")
}

func unavailable(cause error) error {
	if cause == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, cause)
}

func filteredHeaders(headers http.Header) http.Header {
	result := make(http.Header)
	for _, name := range []string{"Content-Type", "Cache-Control"} {
		if value := headers.Get(name); len(value) > 0 && len(value) <= 1024 {
			result.Set(name, value)
		}
	}
	return result
}
