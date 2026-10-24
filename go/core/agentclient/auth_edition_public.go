//go:build !meadow_protected

package agentclient

import "github.com/hey-meadow/meadow-code/go/core/buildinfo"

func productAuthEditionAvailable() bool { return buildinfo.DesktopPackageMode == "portable" }
