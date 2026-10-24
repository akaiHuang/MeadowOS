package protectedbootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"math/big"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/resourcevault"
)

type rsaKeyUnwrapper struct {
	mu        sync.Mutex
	key       *rsa.PrivateKey
	keyID     string
	destroyed bool
}

func newRSAKeyUnwrapper(key *rsa.PrivateKey, keyID string) (*rsaKeyUnwrapper, error) {
	if key == nil || !validIdentifier(keyID, 128) {
		return nil, ErrDenied
	}
	return &rsaKeyUnwrapper{key: key, keyID: keyID}, nil
}

func (unwrapper *rsaKeyUnwrapper) UnwrapKey(ctx context.Context, request resourcevault.KeyUnwrapRequest) ([]byte, error) {
	if unwrapper == nil || ctx == nil || ctx.Err() != nil ||
		request.KeyID != unwrapper.keyID ||
		request.Algorithm != resourcevault.KeyWrapAlgorithm(resourcevault.KeyWrapAlgorithmRSAOAEP256) ||
		len(request.WrappedKey) == 0 {
		return nil, ErrDenied
	}
	unwrapper.mu.Lock()
	defer unwrapper.mu.Unlock()
	if unwrapper.destroyed || unwrapper.key == nil {
		return nil, ErrClosed
	}
	plaintext, err := rsa.DecryptOAEP(
		sha256.New(),
		rand.Reader,
		unwrapper.key,
		request.WrappedKey,
		request.BindingSHA256[:],
	)
	if err != nil || len(plaintext) != 32 {
		clear(plaintext)
		return nil, ErrDenied
	}
	if err := ctx.Err(); err != nil {
		clear(plaintext)
		return nil, err
	}
	return plaintext, nil
}

func (unwrapper *rsaKeyUnwrapper) Close() {
	if unwrapper == nil {
		return
	}
	unwrapper.mu.Lock()
	defer unwrapper.mu.Unlock()
	if unwrapper.destroyed {
		return
	}
	wipeRSAKey(unwrapper.key)
	unwrapper.key = nil
	unwrapper.keyID = ""
	unwrapper.destroyed = true
}

func wipeRSAKey(key *rsa.PrivateKey) {
	if key == nil {
		return
	}
	wipeBigInt(key.N)
	wipeBigInt(key.D)
	for _, prime := range key.Primes {
		wipeBigInt(prime)
	}
	wipeBigInt(key.Precomputed.Dp)
	wipeBigInt(key.Precomputed.Dq)
	wipeBigInt(key.Precomputed.Qinv)
	for index := range key.Precomputed.CRTValues {
		wipeBigInt(key.Precomputed.CRTValues[index].Exp)
		wipeBigInt(key.Precomputed.CRTValues[index].Coeff)
		wipeBigInt(key.Precomputed.CRTValues[index].R)
	}
	key.N = nil
	key.E = 0
	key.D = nil
	key.Primes = nil
	key.Precomputed = rsa.PrecomputedValues{}
}

func wipeBigInt(value *big.Int) {
	if value == nil {
		return
	}
	words := value.Bits()
	clear(words)
	value.SetInt64(0)
}
