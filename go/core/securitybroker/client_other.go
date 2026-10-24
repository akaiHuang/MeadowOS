//go:build !darwin || !cgo

package securitybroker

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

const ProductUpdateStateMaxBytes = 65536

var ErrProductUpdateStateConflict = errors.New("Security Broker product-update state compare-and-swap conflict")
var ErrModelCatalogReplay = errors.New("Security Broker model-catalog sequence replay or rollback")

type ProductUpdateState struct {
	Version uint64
	Digest  [sha256.Size]byte
	Blob    []byte
}

type ModelCatalogState struct {
	Version  uint64
	Sequence uint64
	Digest   [sha256.Size]byte
}

var BuildServiceName = ""

func Secret(_ string) (string, error) {
	return "", fmt.Errorf("Meadow Security Broker requires macOS with CGO")
}

func ReadProductUpdateState() (ProductUpdateState, error) {
	return ProductUpdateState{}, fmt.Errorf("Meadow Security Broker requires macOS with CGO")
}

func CompareAndSwapProductUpdateState(
	_ uint64,
	_ [sha256.Size]byte,
	_ []byte,
) (ProductUpdateState, error) {
	return ProductUpdateState{}, fmt.Errorf("Meadow Security Broker requires macOS with CGO")
}

func ReadModelCatalogState() (ModelCatalogState, error) {
	return ModelCatalogState{}, fmt.Errorf("Meadow Security Broker requires macOS with CGO")
}

func AdvanceModelCatalogState(
	_ uint64,
	_ [sha256.Size]byte,
) (ModelCatalogState, error) {
	return ModelCatalogState{}, fmt.Errorf("Meadow Security Broker requires macOS with CGO")
}
