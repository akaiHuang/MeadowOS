package agentclient

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func newAuthRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate request identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
