package keybroker

import (
	"strings"
	"unicode/utf8"
)

// NewKeychain creates a production broker backed directly by the macOS
// Security framework. The service name is metadata and must not contain a key,
// token, device identifier, or other secret.
func NewKeychain(service string, binding DeviceBinding) (*Broker, error) {
	if service == "" || len(service) > 255 || !utf8.ValidString(service) ||
		strings.TrimSpace(service) != service || strings.IndexByte(service, 0) >= 0 ||
		isZeroDigest(binding.digest) {
		return nil, ErrInvalidArgument
	}
	executor, err := newPlatformKeychainExecutor(service)
	if err != nil {
		return nil, err
	}
	return newBroker(executor, binding)
}
