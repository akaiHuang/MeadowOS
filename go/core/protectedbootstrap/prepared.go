package protectedbootstrap

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/keybroker"
	"github.com/hey-meadow/meadow-code/go/core/modelprotection"
	"github.com/hey-meadow/meadow-code/go/core/resourcevault"
)

// Prepared owns every secret-bearing component created by Prepare.
type Prepared struct {
	mu             sync.Mutex
	closed         bool
	closeDone      chan struct{}
	closeErr       error
	broker         *keybroker.Broker
	modelReader    *modelprotection.Reader
	modelWrapper   *modelprotection.AESGCMKeyWrapper
	vault          *resourcevault.Vault
	vaultUnwrapper *rsaKeyUnwrapper
	plaintexts     map[*ResourcePlaintext]struct{}
	status         Status
	modelInfo      ModelInfo
}

func newPrepared() *Prepared {
	return &Prepared{
		closeDone:  make(chan struct{}),
		plaintexts: make(map[*ResourcePlaintext]struct{}),
	}
}

func (p *Prepared) Status() (Status, error) {
	if p == nil {
		return Status{}, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.modelReader == nil || p.vault == nil {
		return Status{}, ErrClosed
	}
	return p.status, nil
}

func (p *Prepared) ModelInfo() (ModelInfo, error) {
	if p == nil {
		return ModelInfo{}, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.modelReader == nil {
		return ModelInfo{}, ErrClosed
	}
	return p.modelInfo, nil
}

func (p *Prepared) ReadModel(destination []byte) (int, error) {
	if p == nil {
		return 0, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.modelReader == nil {
		return 0, ErrClosed
	}
	return p.modelReader.Read(destination)
}

func (p *Prepared) OpenResource(ctx context.Context, request resourcevault.ResourceRequest, payload io.Reader) (*ResourcePlaintext, error) {
	if p == nil {
		return nil, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.vault == nil {
		return nil, ErrClosed
	}
	plaintext, err := p.vault.Open(ctx, request, payload)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, err
		case errors.Is(err, resourcevault.ErrNotFound):
			return nil, resourcevault.ErrNotFound
		case errors.Is(err, resourcevault.ErrUnauthorized):
			return nil, resourcevault.ErrUnauthorized
		default:
			return nil, ErrDenied
		}
	}
	owned := &ResourcePlaintext{owner: p, plaintext: plaintext}
	p.plaintexts[owned] = struct{}{}
	return owned, nil
}

func (p *Prepared) unregister(value *ResourcePlaintext) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.plaintexts, value)
	p.mu.Unlock()
}

func (p *Prepared) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closeDone == nil {
		p.closeDone = make(chan struct{})
	}
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		<-done
		p.mu.Lock()
		err := p.closeErr
		p.mu.Unlock()
		return err
	}
	p.closed = true
	done := p.closeDone
	plaintexts := make([]*ResourcePlaintext, 0, len(p.plaintexts))
	for value := range p.plaintexts {
		plaintexts = append(plaintexts, value)
	}
	p.plaintexts = nil
	modelReader := p.modelReader
	modelWrapper := p.modelWrapper
	vaultUnwrapper := p.vaultUnwrapper
	broker := p.broker
	p.modelReader = nil
	p.modelWrapper = nil
	p.vault = nil
	p.vaultUnwrapper = nil
	p.broker = nil
	p.mu.Unlock()

	failed := false
	for _, value := range plaintexts {
		if err := value.closeDetached(); err != nil {
			failed = true
		}
	}
	if modelReader != nil {
		if err := modelReader.Close(); err != nil {
			failed = true
		}
	}
	if modelWrapper != nil {
		modelWrapper.Destroy()
	}
	if vaultUnwrapper != nil {
		vaultUnwrapper.Close()
	}
	if broker != nil {
		if err := broker.Close(); err != nil {
			failed = true
		}
	}
	var closeErr error
	if failed {
		closeErr = ErrCloseFailed
	}
	p.mu.Lock()
	p.closeErr = closeErr
	close(done)
	p.mu.Unlock()
	return closeErr
}

// ResourcePlaintext is owned by Prepared and is zeroed by either Close method.
type ResourcePlaintext struct {
	mu        sync.Mutex
	owner     *Prepared
	plaintext *resourcevault.Plaintext
	closed    bool
}

func (r *ResourcePlaintext) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.plaintext == nil {
		return 0
	}
	return r.plaintext.Len()
}

func (r *ResourcePlaintext) Use(fn func([]byte) error) error {
	if r == nil || fn == nil {
		return ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.plaintext == nil {
		return ErrClosed
	}
	return r.plaintext.Use(fn)
}

func (r *ResourcePlaintext) Close() error {
	return r.closeDetached()
}

func (r *ResourcePlaintext) closeDetached() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	owner := r.owner
	plaintext := r.plaintext
	r.owner = nil
	r.plaintext = nil
	r.mu.Unlock()
	var err error
	if plaintext != nil {
		err = plaintext.Close()
	}
	if owner != nil {
		owner.unregister(r)
	}
	return err
}
