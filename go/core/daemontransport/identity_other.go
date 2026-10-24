//go:build !darwin

package daemontransport

import (
	"fmt"
	"os"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/productsecurity"
)

func verifyDaemonIdentity(_ string, _ string) error {
	if productsecurity.Required() || strings.TrimSpace(os.Getenv("MEADOW_PUBLIC_RELEASE")) == "1" {
		return fmt.Errorf("public Meadow Agent identity verification is only implemented for macOS")
	}
	return nil
}
