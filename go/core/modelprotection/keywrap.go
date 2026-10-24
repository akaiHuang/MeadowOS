package modelprotection

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"sync"
)

const keyWrapAAD = "meadow/model-protection/key-wrap/aes-256-gcm/v1\x00"

var keyWrapMagic = [4]byte{'M', 'K', 'W', 1}

// AESGCMKeyWrapper wraps container data keys under a 256-bit key-encryption
// key. It implements both KeyWrapper and KeyUnwrapper.
type AESGCMKeyWrapper struct {
	mu        sync.Mutex
	key       [DataKeySize]byte
	destroyed bool
}

// NewAESGCMKeyWrapper copies a 32-byte key-encryption key into a wrapper.
func NewAESGCMKeyWrapper(key []byte) (*AESGCMKeyWrapper, error) {
	if len(key) != DataKeySize {
		return nil, fmt.Errorf("%w: key-encryption key must be %d bytes", ErrInvalidArgument, DataKeySize)
	}
	wrapper := &AESGCMKeyWrapper{}
	copy(wrapper.key[:], key)
	return wrapper, nil
}

// WrapKey encrypts exactly one 32-byte data key with AES-256-GCM and a random
// nonce. The returned envelope is versioned and self-contained.
func (wrapper *AESGCMKeyWrapper) WrapKey(ctx context.Context, dataKey []byte) ([]byte, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if len(dataKey) != DataKeySize {
		return nil, fmt.Errorf("%w: data key must be %d bytes", ErrKeyWrap, DataKeySize)
	}
	wrapper.mu.Lock()
	defer wrapper.mu.Unlock()
	if wrapper.destroyed {
		return nil, fmt.Errorf("%w: wrapper was destroyed", ErrKeyWrap)
	}
	aead, err := wrapper.newAEAD()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyWrap, err)
	}
	prefixSize := len(keyWrapMagic) + aead.NonceSize()
	envelope := make([]byte, prefixSize, prefixSize+DataKeySize+aead.Overhead())
	copy(envelope, keyWrapMagic[:])
	nonce := envelope[len(keyWrapMagic):prefixSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: nonce generation: %v", ErrKeyWrap, err)
	}
	envelope = aead.Seal(envelope, nonce, dataKey, []byte(keyWrapAAD))
	return envelope, nil
}

// UnwrapKey authenticates and decrypts a data-key envelope.
func (wrapper *AESGCMKeyWrapper) UnwrapKey(ctx context.Context, envelope []byte) ([]byte, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	wrapper.mu.Lock()
	defer wrapper.mu.Unlock()
	if wrapper.destroyed {
		return nil, fmt.Errorf("%w: wrapper was destroyed", ErrKeyUnwrap)
	}
	aead, err := wrapper.newAEAD()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyUnwrap, err)
	}
	expectedSize := len(keyWrapMagic) + aead.NonceSize() + DataKeySize + aead.Overhead()
	if len(envelope) != expectedSize ||
		subtle.ConstantTimeCompare(envelope[:len(keyWrapMagic)], keyWrapMagic[:]) != 1 {
		return nil, ErrKeyUnwrap
	}
	nonceStart := len(keyWrapMagic)
	ciphertextStart := nonceStart + aead.NonceSize()
	dataKey, err := aead.Open(nil, envelope[nonceStart:ciphertextStart], envelope[ciphertextStart:], []byte(keyWrapAAD))
	if err != nil || len(dataKey) != DataKeySize {
		clear(dataKey)
		return nil, ErrKeyUnwrap
	}
	return dataKey, nil
}

// Destroy clears the retained key-encryption key. The wrapper cannot be reused.
func (wrapper *AESGCMKeyWrapper) Destroy() {
	if wrapper == nil {
		return
	}
	wrapper.mu.Lock()
	defer wrapper.mu.Unlock()
	clear(wrapper.key[:])
	wrapper.destroyed = true
}

func (wrapper *AESGCMKeyWrapper) newAEAD() (cipher.AEAD, error) {
	block, err := aes.NewCipher(wrapper.key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
