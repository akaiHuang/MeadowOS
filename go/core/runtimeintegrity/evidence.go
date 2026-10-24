// Package runtimeintegrity produces fail-closed evidence about the currently
// executing Meadow application. It verifies integrity; it never grants an
// entitlement or authorizes an operation.
package runtimeintegrity

import "fmt"

// EvidenceSchemaVersion identifies the shape of Evidence.
const EvidenceSchemaVersion = 1

// Check identifies the verification boundary that rejected the runtime.
type Check string

const (
	CheckPlatform          Check = "platform"
	CheckEnvironment       Check = "environment"
	CheckBundleLayout      Check = "bundle_layout"
	CheckDirectFile        Check = "direct_file"
	CheckInfoPlist         Check = "info_plist"
	CheckMachO             Check = "mach_o"
	CheckCodeSignature     Check = "code_signature"
	CheckCodeIdentity      Check = "code_identity"
	CheckManifestSignature Check = "manifest_signature"
	CheckManifestSchema    Check = "manifest_schema"
	CheckComponentHash     Check = "component_hash"
)

// VerificationError reports the exact boundary that failed. Callers must
// treat every VerificationError, and every other returned error, as a denial
// of runtime integrity.
type VerificationError struct {
	Check Check
	Path  string
	Cause error
}

func (e *VerificationError) Error() string {
	if e == nil {
		return "runtime integrity verification failed"
	}
	if e.Path == "" {
		return fmt.Sprintf("runtime integrity %s check failed: %v", e.Check, e.Cause)
	}
	return fmt.Sprintf("runtime integrity %s check failed for %s: %v", e.Check, e.Path, e.Cause)
}

func (e *VerificationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Evidence is returned only after every integrity check succeeds. It contains
// no authorization or entitlement field by design.
type Evidence struct {
	SchemaVersion int                    `json:"schema_version"`
	Application   ApplicationEvidence    `json:"application"`
	Manifest      ManifestEvidence       `json:"manifest"`
	Executables   []ExecutableEvidence   `json:"executables"`
	Environment   EnvironmentEvidence    `json:"environment"`
}

// ApplicationEvidence binds the canonical App bundle, Info.plist identity,
// outer code signature, and currently loaded main executable.
type ApplicationEvidence struct {
	BundlePath        string `json:"bundle_path"`
	ContentsPath      string `json:"contents_path"`
	BundleIdentifier string `json:"bundle_identifier"`
	TeamIdentifier   string `json:"team_identifier"`
	CodeIdentifier   string `json:"code_identifier"`
	ExecutablePath   string `json:"executable_path"`
}

// ManifestEvidence identifies the exact vendor-signed product manifest bytes.
type ManifestEvidence struct {
	Path                  string `json:"path"`
	Bytes                 int64  `json:"bytes"`
	SHA256                string `json:"sha256"`
	SignaturePath         string `json:"signature_path"`
	SignatureSHA256       string `json:"signature_sha256"`
	KeyID                 string `json:"key_id"`
	ProductSchemaVersion  int    `json:"product_schema_version"`
	BinaryCount           int    `json:"binary_count"`
	ComponentCount        int    `json:"component_count"`
}

// ExecutableEvidence binds one loaded executable or helper to its bytes and
// native code-signing identity. The application entry is code-signature
// protected but intentionally absent from the component hash manifest because
// hashing it would create a circular dependency with the outer App signature.
type ExecutableEvidence struct {
	Role                string `json:"role"`
	Path                string `json:"path"`
	Bytes               int64  `json:"bytes"`
	SHA256              string `json:"sha256"`
	CodeIdentifier      string `json:"code_identifier"`
	TeamIdentifier      string `json:"team_identifier"`
	ComponentID         string `json:"component_id,omitempty"`
	ComponentIdentifier string `json:"component_identifier,omitempty"`
}

// EnvironmentEvidence records the size of the environment inspected for
// injection controls. Verify returns an error instead of Evidence when any
// DYLD-prefixed variable is present.
type EnvironmentEvidence struct {
	VariableCount int `json:"variable_count"`
}

// Verify strictly verifies the current process and App bundle. It has no
// permissive development mode and accepts no caller-supplied trust root, path,
// identity, bypass, or authorization decision. Evidence is zero on failure.
func Verify() (Evidence, error) {
	evidence, err := verifyPlatform()
	if err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func failure(check Check, path string, cause error) error {
	return &VerificationError{Check: check, Path: path, Cause: cause}
}
