//go:build !darwin

package protectedbootstrap

import (
	"os"

	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
)

func openVerifiedModelAsset(_ string, _ productsecurity.VerifiedModelAsset) (*os.File, error) {
	return nil, ErrDenied
}
