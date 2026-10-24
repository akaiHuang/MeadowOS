// Package buildinfo owns the public identity of the native Go product.
package buildinfo

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// CoreBuildIdentity binds a packaged Desktop to its companion Core launcher.
// BuildDate distinguishes local rebuilds with uncommitted changes at one SHA.
// Unpackaged go run builds do not claim a release identity.
func CoreBuildIdentity() string {
	if strings.TrimSpace(BuildDate) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{DesktopBuild, Commit, BuildDate}, "\x00")))
	return fmt.Sprintf("sha256:%x", digest)
}

const (
	ProductName = "Meadow cli"
	Edition     = "H_Go"
)

// Version is intentionally linker-overridable for release builds:
//
//	go build -ldflags "-X github.com/hey-meadow/meadow-code/go/core/buildinfo.Version=H_Go_V.0.1"
var Version = "H_Go_V.0.1"

// DesktopProductName, DesktopVersion, and DesktopBuild own the native desktop
// identity independently from the CLI compatibility label above. Release
// packaging overrides the version and build values with linker flags.
var (
	DesktopProductName = "Meadow Desktop"
	DesktopVersion     = "0.1"
	DesktopBuild       = "0.1"
	// DesktopPackageMode describes immutable bundle composition. Portable
	// builds have no local inference runtime; an inherited edition environment
	// must never turn their startup into the legacy embedded-Core path.
	DesktopPackageMode = "diagnostic"
	DesktopVariant     = ""
	// DesktopBundledModelIDs is linker-populated by the self-contained
	// product packager. An empty value preserves the developer build's
	// downloadable multi-model catalog.
	DesktopBundledModelIDs = ""
)

var (
	Commit    = ""
	BuildDate = ""
	// ProtectedProductAuthOrigin is populated only by the protected release
	// builder from the validated linked product-auth configuration.
	ProtectedProductAuthOrigin = ""
)

// Label is the exact reader-facing product/version identity.
func Label() string {
	return ProductName + " " + Version
}

// DetailedLabel adds optional release metadata without changing the stable
// product/version prefix scripts and users rely on.
func DetailedLabel() string {
	parts := []string{Label()}
	if value := strings.TrimSpace(Commit); value != "" {
		parts = append(parts, "commit "+value)
	}
	if value := strings.TrimSpace(BuildDate); value != "" {
		parts = append(parts, "built "+value)
	}
	return strings.Join(parts, " · ")
}

// DesktopAboutLabel is the exact title shown by the native About panel.
func DesktopAboutLabel() string {
	return DesktopProductName + " v" + DesktopVersion + "(" + DesktopBuild + ")"
}

// DesktopHasBundledModels reports whether this binary belongs to a
// self-contained product bundle. Product code uses this boundary to disable
// model downloads and fail closed instead of falling back to a developer path.
func DesktopHasBundledModels() bool {
	return strings.TrimSpace(DesktopBundledModelIDs) != ""
}

// DesktopBundledModels returns the stable model IDs physically included in
// the app. The linker value is a comma-separated list to keep -ldflags simple.
func DesktopBundledModels() []string {
	if !DesktopHasBundledModels() {
		return nil
	}
	seen := map[string]bool{}
	models := make([]string, 0)
	for _, value := range strings.Split(DesktopBundledModelIDs, ",") {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		models = append(models, value)
	}
	return models
}
