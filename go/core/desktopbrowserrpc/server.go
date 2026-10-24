package desktopbrowserrpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

func Serve(ctx context.Context, conn net.Conn, secret []byte, handler http.Handler) error {
	return ServeWithReady(ctx, conn, secret, handler, nil)
}

// ServeWithReady reports completion of the authenticated handshake. A launcher
// must combine this capability with the exact spawned Agent's health identity.
func ServeWithReady(ctx context.Context, conn net.Conn, secret []byte, handler http.Handler, ready func()) error {
	if conn == nil || handler == nil || len(secret) != PeerSecretBytes {
		if conn != nil {
			_ = conn.Close()
		}
		return ErrAuthentication
	}
	if ctx == nil {
		ctx = context.Background()
	}
	serverCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	stopClose := context.AfterFunc(serverCtx, func() { _ = conn.Close() })
	defer stopClose()
	deadline := time.Now().Add(ioTimeout)
	if caller, ok := ctx.Deadline(); ok && caller.Before(deadline) {
		deadline = caller
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return unavailable(err)
	}
	hello, err := readFrame(conn, maxMetadataBytes)
	if err != nil || hello.Kind != "hello" || hello.ID != 0 || !validProof(secret, "client", hello.Nonce, hello.Proof) {
		return ErrAuthentication
	}
	writer := newFrameWriter(conn)
	if err := writer.write(serverCtx, frame{Kind: "ack", Nonce: hello.Nonce, Proof: authProof(secret, "server", hello.Nonce)}, maxMetadataBytes); err != nil {
		return unavailable(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return unavailable(err)
	}
	if ready != nil {
		ready()
	}
	var mu sync.Mutex
	active := make(map[uint64]context.CancelFunc)
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, cancel := range active {
			cancel()
		}
	}()
	var lastID uint64
	for {
		value, err := readFrame(conn, maxRequestFrame)
		if err != nil {
			return unavailable(err)
		}
		switch value.Kind {
		case "cancel":
			if value.ID == 0 || value.ID > lastID {
				return ErrInvalid
			}
			mu.Lock()
			cancel := active[value.ID]
			mu.Unlock()
			if cancel != nil {
				cancel()
			}
		case "request":
			if value.ID <= lastID || !validOwner(value.SessionID, value.WorkspaceRoot) || !validPageID(value.PageID) || !validRoute(value.Method, value.Path) || len(value.Body) > MaxRequestBytes {
				return ErrInvalid
			}
			lastID = value.ID
			mu.Lock()
			if len(active) >= MaxActive {
				mu.Unlock()
				if err := writer.write(serverCtx, frame{Kind: "result", ID: value.ID, Status: http.StatusTooManyRequests, Body: []byte(`{"error":"browser_busy"}`)}, maxResponseFrame); err != nil {
					return unavailable(err)
				}
				continue
			}
			requestCtx, requestCancel := context.WithTimeout(serverCtx, 20*time.Second)
			active[value.ID] = requestCancel
			mu.Unlock()
			go func(value frame) {
				defer func() { requestCancel(); mu.Lock(); delete(active, value.ID); mu.Unlock() }()
				if requestCtx.Err() != nil {
					return
				}
				request, err := http.NewRequestWithContext(requestCtx, value.Method, value.Path, bytes.NewReader(value.Body))
				if err != nil {
					return
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(HeaderSession, value.SessionID)
				request.Header.Set(HeaderWorkspace, value.WorkspaceRoot)
				request.Header.Set(HeaderPage, value.PageID)
				request.Host = "browser"
				response := &boundedResponse{headers: make(http.Header)}
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							response = &boundedResponse{headers: make(http.Header), status: http.StatusInternalServerError}
							_, _ = response.Write([]byte(`{"error":"browser_environment_unavailable"}`))
						}
					}()
					handler.ServeHTTP(response, request)
				}()
				if requestCtx.Err() != nil {
					return
				}
				status := response.status
				if status == 0 {
					status = http.StatusOK
				}
				body := response.body.Bytes()
				if response.overflow {
					status = http.StatusRequestEntityTooLarge
					body = []byte(`{"error":"browser_response_limit"}`)
				}
				if err := writer.write(requestCtx, frame{Kind: "result", ID: value.ID, Status: status, Headers: filteredHeaders(response.headers), Body: body}, maxResponseFrame); err != nil && requestCtx.Err() == nil {
					cancel()
				}
			}(value)
		default:
			return ErrInvalid
		}
	}
}

type boundedResponse struct {
	headers  http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (writer *boundedResponse) Header() http.Header { return writer.headers }
func (writer *boundedResponse) WriteHeader(status int) {
	if writer.status == 0 {
		if status < 200 || status > 599 {
			status = http.StatusInternalServerError
		}
		writer.status = status
	}
}
func (writer *boundedResponse) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if writer.overflow || len(body) > MaxResponseBytes-writer.body.Len() {
		writer.overflow = true
		return 0, ErrLimit
	}
	return writer.body.Write(body)
}
func (writer *boundedResponse) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{writer}, io.LimitReader(reader, MaxResponseBytes+1))
}
func (writer *boundedResponse) Flush() {}

func ValidateDescriptors(fd, secretFD int) error {
	if fd == 0 && secretFD == 0 {
		return nil
	}
	if fd != 3 || secretFD != 4 {
		return fmt.Errorf("%w: embedded Browser descriptors must be inherited FD 3 and 4", ErrInvalid)
	}
	return nil
}
