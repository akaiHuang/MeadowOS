//go:build !darwin || !cgo

package keybroker

func newPlatformKeychainExecutor(_ string) (commandExecutor, error) {
	return nil, ErrUnavailable
}
