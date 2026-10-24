package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/agentdaemon"
)

// Agent HTTP requests own short-lived observations such as git status. An
// ordinary http.Server.Shutdown waits for handlers without cancelling their
// contexts, so it can leave those subprocesses alive when Desktop's supervisor
// eventually terminates the helper. Give every connection the helper's lifetime
// and cancel it before draining; CommandContext can then kill and reap its own
// exact child while the helper is still running.
type agentHTTPServer struct {
	*http.Server
	cancelRequests    context.CancelFunc
	closeSessionOwner func() error
	closeOnce         sync.Once
	closeError        error
	closeDone         chan struct{}
}

func newAgentHTTPServer(handler http.Handler, sessionOwners ...http.Handler) *agentHTTPServer {
	requestContext, cancelRequests := context.WithCancel(context.Background())
	server := &agentHTTPServer{
		Server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    agentdaemon.MaxHeaderBytes,
			BaseContext:       func(net.Listener) context.Context { return requestContext },
		},
		cancelRequests: cancelRequests,
		closeDone:      make(chan struct{}),
	}
	for _, candidate := range append(sessionOwners, handler) {
		if owner, ok := candidate.(interface{ Close() error }); ok {
			server.closeSessionOwner = owner.Close
			break
		}
	}
	return server
}

func (server *agentHTTPServer) Shutdown(ctx context.Context) error {
	server.cancelRequests()
	server.closeOnce.Do(func() {
		go func() {
			defer close(server.closeDone)
			if server.closeSessionOwner != nil {
				server.closeError = server.closeSessionOwner()
			}
		}()
	})
	select {
	case <-server.closeDone:
	case <-ctx.Done():
		return errors.Join(ctx.Err(), server.Server.Close())
	}
	if err := server.Server.Shutdown(ctx); err != nil {
		// A handler which ignores cancellation must not leave a live accepted
		// connection after the bounded drain. This closes only this server's
		// connections; it neither signals other processes nor touches Core.
		return errors.Join(server.closeError, err, server.Server.Close())
	}
	return server.closeError
}
