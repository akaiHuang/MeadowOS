package desktopbrowserrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type reply struct {
	value frame
	err   error
}

type Client struct {
	conn      net.Conn
	writer    *frameWriter
	mu        sync.Mutex
	nextID    uint64
	pending   map[uint64]chan reply
	failure   error
	done      chan struct{}
	closeOnce sync.Once
	active    chan struct{}
	bindings  map[ownerBinding]*pageBinding
}

type ownerBinding struct{ session, workspace string }
type pageBinding struct {
	mu        sync.Mutex
	id        string
	revision  uint64
	operation chan struct{}
}

func newPageBinding() *pageBinding { return &pageBinding{operation: make(chan struct{}, 1)} }

type pageContextKey struct{}
type operationPins struct {
	mu   sync.Mutex
	page map[ownerBinding]pageReceipt
}

type pageReceipt struct {
	id        string
	confirmed bool
}

// WithPageContext keeps one tool execution's page receipt across its separate
// Browser requests. Another execution may navigate the shared view; the older
// execution must retain its original page ID and fail the native page check.
func WithPageContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, pageContextKey{}, &operationPins{page: make(map[ownerBinding]pageReceipt)})
}

func NewClient(ctx context.Context, conn net.Conn, secret []byte) (*Client, error) {
	if conn == nil || len(secret) != PeerSecretBytes {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, ErrAuthentication
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	deadline := time.Now().Add(ioTimeout)
	if caller, ok := ctx.Deadline(); ok && caller.Before(deadline) {
		deadline = caller
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, unavailable(err)
	}
	nonceBytes := make([]byte, PeerSecretBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		_ = conn.Close()
		return nil, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	writer := newFrameWriter(conn)
	if err := writer.write(ctx, frame{Kind: "hello", Nonce: nonce, Proof: authProof(secret, "client", nonce)}, maxMetadataBytes); err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable(err)
	}
	ack, err := readFrame(conn, maxMetadataBytes)
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	if err != nil || ack.Kind != "ack" || ack.ID != 0 || ack.Nonce != nonce || !validProof(secret, "server", nonce, ack.Proof) {
		_ = conn.Close()
		return nil, ErrAuthentication
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, unavailable(err)
	}
	client := &Client{conn: conn, writer: newFrameWriter(conn), pending: make(map[uint64]chan reply), done: make(chan struct{}), active: make(chan struct{}, MaxActive), bindings: make(map[ownerBinding]*pageBinding)}
	go client.readLoop()
	return client, nil
}

func (client *Client) Factory(sessionID, workspaceRoot string) *http.Client {
	page := newPageBinding()
	if client != nil {
		client.mu.Lock()
		key := ownerBinding{sessionID, workspaceRoot}
		if existing := client.bindings[key]; existing != nil {
			page = existing
		} else {
			client.bindings[key] = page
		}
		client.mu.Unlock()
	}
	return &http.Client{Transport: boundTransport{client: client, session: sessionID, workspace: workspaceRoot, page: page}, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type boundTransport struct {
	client             *Client
	session, workspace string
	page               *pageBinding
}

func (transport boundTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || !validPrivateURL(request.URL) || !validRoute(request.Method, request.URL.RequestURI()) || !validOwner(transport.session, transport.workspace) {
		return nil, ErrInvalid
	}
	if transport.client == nil {
		return nil, ErrUnavailable
	}
	// Keep navigation/discovery, its receipt, and the following page operation
	// in one owner order. Parallel subtasks share this binding; a later native
	// navigation cannot execute before an earlier receipt updates its pin.
	select {
	case transport.page.operation <- struct{}{}:
		defer func() { <-transport.page.operation }()
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	transport.page.mu.Lock()
	pageID := transport.page.id
	transport.page.mu.Unlock()
	localPins, _ := request.Context().Value(pageContextKey{}).(*operationPins)
	key := ownerBinding{transport.session, transport.workspace}
	localConfirmed := false
	if localPins != nil {
		localPins.mu.Lock()
		if localPage, exists := localPins.page[key]; exists {
			pageID, localConfirmed = localPage.id, localPage.confirmed
		} else {
			localPins.page[key] = pageReceipt{id: pageID}
		}
		localPins.mu.Unlock()
	}
	// Discovery after this execution's navigation is still evidence from
	// that exact page. Only a fresh discovery scope may capture a new page.
	unpinnedDiscovery := request.URL.Path == "/webmcp/tools" && request.URL.Query().Get("page_id") == "" && !localConfirmed
	refresh := request.URL.Path == "/goto" || unpinnedDiscovery
	transport.page.mu.Lock()
	if refresh {
		transport.page.revision++
	}
	revision := transport.page.revision
	transport.page.mu.Unlock()
	if request.URL.Path == "/health" || request.URL.Path == "/goto" || unpinnedDiscovery {
		pageID = ""
	} else if pageID == "" {
		return nil, ErrPageContext
	}
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(request.Body, MaxRequestBytes+1))
		_ = request.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > MaxRequestBytes {
			return nil, ErrLimit
		}
	}
	if request.URL.Path == "/webmcp/tools" || request.URL.Path == "/webmcp/execute" {
		explicit := request.URL.Query().Get("page_id")
		if len(body) > 0 {
			var pins struct {
				PageID string `json:"page_id"`
			}
			if json.Unmarshal(body, &pins) != nil {
				return nil, ErrInvalid
			}
			if pins.PageID != "" {
				explicit = pins.PageID
			}
		}
		if explicit != "" && (pageID == "" || explicit != pageID) {
			return nil, ErrInvalid
		}
	}
	value, err := transport.client.call(request.Context(), frame{Kind: "request", SessionID: transport.session, WorkspaceRoot: transport.workspace, PageID: pageID, Method: request.Method, Path: request.URL.RequestURI(), Body: body})
	if err != nil {
		return nil, err
	}
	if value.Status >= 200 && value.Status < 300 && (request.URL.Path == "/goto" || request.URL.Path == "/webmcp/tools") {
		var pins struct {
			OK     bool   `json:"ok"`
			PageID string `json:"page_id"`
		}
		if json.Unmarshal(value.Body, &pins) == nil && pins.OK {
			if pins.PageID == "" || !validPageID(pins.PageID) {
				return nil, ErrInvalid
			}
			if request.URL.Path == "/webmcp/tools" && pageID != "" && pins.PageID != pageID {
				return nil, ErrInvalid
			}
			transport.page.mu.Lock()
			if revision != transport.page.revision {
				transport.page.mu.Unlock()
				return nil, ErrInvalid
			}
			transport.page.id = pins.PageID
			transport.page.mu.Unlock()
			if localPins != nil {
				localPins.mu.Lock()
				localPins.page[key] = pageReceipt{id: pins.PageID, confirmed: true}
				localPins.mu.Unlock()
			}
		}
	}
	return &http.Response{StatusCode: value.Status, Header: filteredHeaders(value.Headers), Body: io.NopCloser(bytes.NewReader(value.Body)), ContentLength: int64(len(value.Body)), Request: request}, nil
}

func (client *Client) call(ctx context.Context, value frame) (frame, error) {
	select {
	case client.active <- struct{}{}:
		defer func() { <-client.active }()
	case <-ctx.Done():
		return frame{}, ctx.Err()
	case <-client.done:
		return frame{}, client.err()
	}
	// IDs are allocated under the same lock as writes. Requests are strictly
	// increasing on the wire, so the server rejects replay without an unbounded
	// history. Cancellation for an already-written ID remains independently framed.
	if err := client.writer.acquire(ctx); err != nil {
		return frame{}, err
	}
	client.mu.Lock()
	if client.failure != nil {
		err := client.failure
		client.mu.Unlock()
		client.writer.release()
		return frame{}, err
	}
	client.nextID++
	if client.nextID == 0 {
		client.mu.Unlock()
		client.writer.release()
		client.fail(ErrLimit)
		return frame{}, ErrLimit
	}
	value.ID = client.nextID
	response := make(chan reply, 1)
	client.pending[value.ID] = response
	client.mu.Unlock()
	err := client.writer.writeLocked(ctx, value, maxRequestFrame)
	client.writer.release()
	if err != nil {
		client.fail(unavailable(err))
		if ctx.Err() != nil {
			return frame{}, ctx.Err()
		}
		return frame{}, unavailable(err)
	}
	select {
	case result := <-response:
		return result.value, result.err
	case <-ctx.Done():
		client.mu.Lock()
		delete(client.pending, value.ID)
		client.mu.Unlock()
		cancelCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		err := client.writer.write(cancelCtx, frame{Kind: "cancel", ID: value.ID}, maxMetadataBytes)
		cancel()
		if err != nil {
			client.fail(unavailable(err))
		}
		return frame{}, ctx.Err()
	case <-client.done:
		return frame{}, client.err()
	}
}

func (client *Client) readLoop() {
	for {
		value, err := readFrame(client.conn, maxResponseFrame)
		if err != nil {
			client.fail(unavailable(err))
			return
		}
		if value.Kind != "result" || value.ID == 0 || value.Status < 200 || value.Status > 599 || len(value.Body) > MaxResponseBytes {
			client.fail(ErrInvalid)
			return
		}
		client.mu.Lock()
		pending := client.pending[value.ID]
		if value.ID > client.nextID {
			client.mu.Unlock()
			client.fail(ErrInvalid)
			return
		}
		delete(client.pending, value.ID)
		client.mu.Unlock()
		// A cancelled call has no pending entry. Its late result cannot be
		// delivered to another caller and does not trigger a replay.
		if pending != nil {
			pending <- reply{value: value}
		}
	}
}

func (client *Client) err() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.failure != nil {
		return client.failure
	}
	return ErrUnavailable
}

func (client *Client) fail(err error) {
	client.closeOnce.Do(func() {
		client.mu.Lock()
		client.failure = err
		client.pending = make(map[uint64]chan reply)
		client.mu.Unlock()
		close(client.done)
		_ = client.conn.Close()
	})
}

func (client *Client) Close() error {
	if client != nil {
		client.fail(ErrUnavailable)
	}
	return nil
}

// IsUnavailable identifies channel absence/closure without describing it as an
// executable mismatch or application assertion failure.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }
