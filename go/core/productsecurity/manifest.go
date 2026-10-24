// Package productsecurity verifies the vendor-signed compatibility manifest
// embedded in a production Meadow.app bundle. It is deliberately independent
// from licensing: a valid subscription must not make a modified component
// trusted, and a valid component signature must not grant an entitlement.
package productsecurity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
)

// These values are populated only by the protected release packager. Keeping
// development builds permissive preserves local iteration while public builds
// fail closed when the manifest, signature, or a component is missing.
var (
	BuildRequired     = ""
	BuildPublicKeyB64 = ""
	BuildKeyID        = ""
)

const (
	ProductManifestSchemaVersion        = 3
	NativeRuntimeComponentID            = "meadow-native-runtime"
	NativeRuntimeIdentifierSuffix       = ".runtime"
	NativeRuntimeContentsRelativePath = "Helpers/meadow-runtime"
)

var productVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type signatureDocument struct {
	SchemaVersion   int    `json:"schema_version"`
	Algorithm       string `json:"algorithm"`
	KeyID           string `json:"key_id"`
	SignatureBase64 string `json:"signature_base64"`
}

type productDocument struct {
	SchemaVersion           int               `json:"schemaVersion"`
	Product                 string            `json:"product"`
	BundleID                string            `json:"bundleId"`
	Channel                 string            `json:"channel"`
	Version                 string            `json:"version"`
	BuildVersion            string            `json:"buildVersion"`
	Commit                  string            `json:"commit"`
	BuildDate               string            `json:"buildDate"`
	Platform                json.RawMessage   `json:"platform"`
	BundledModelIDs         []string          `json:"bundledModelIds"`
	ModelRoles              json.RawMessage   `json:"modelRoles"`
	ModelDelivery           string            `json:"modelDelivery"`
	NetworkDownloadRequired bool              `json:"networkDownloadRequired"`
	OfflineInference        bool              `json:"offlineInference"`
	ModelManifests          []string          `json:"modelManifests"`
	RuntimeManifest         string            `json:"runtimeManifest"`
	Runtime                 json.RawMessage   `json:"runtime"`
	Licenses                json.RawMessage   `json:"licenses"`
	Binaries                []binaryDocument  `json:"binaries"`
	Components              []componentDocument `json:"components"`
}

type binaryDocument struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

type componentDocument struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Mode       uint32 `json:"mode"`
}

// VerifiedNativeRuntime is the fixed runtime record resolved from the
// Ed25519-verified manifest inside the current application. No caller path,
// identifier, digest, or alternate component selector participates in this
// result.
type VerifiedNativeRuntime struct {
	BundleIdentifier string
	ComponentID string
	ComponentIdentifier string
	ContentsDirectory string
	ContentsRelativePath string
	ManifestSize int64
	ManifestSHA256 [sha256.Size]byte
}

// Required reports whether this binary belongs to a protected public build.
func Required() bool {
	switch strings.ToLower(strings.TrimSpace(BuildRequired)) {
	case "1", "true", "required", "yes":
		return true
	default:
		return false
	}
}

// VerifyCurrentBundle validates the signed manifest and every nested native
// component before the caller starts a session, model runtime, or tool loop.
func VerifyCurrentBundle() error {
	if !Required() {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	contents, err := appContentsForExecutable(executable)
	if err != nil {
		return err
	}
	return verifyBundle(contents)
}

// ResolveCurrentNativeRuntime verifies the signed manifest in the currently
// executing application and resolves only the canonical native runtime entry.
// Unlike VerifyCurrentBundle, this API never becomes permissive in development
// builds: a missing signature, schema field, id, or identifier fails closed.
func ResolveCurrentNativeRuntime() (VerifiedNativeRuntime, error) {
	executable, err := os.Executable()
	if err != nil {
		return VerifiedNativeRuntime{}, fmt.Errorf("resolve executable: %w", err)
	}
	contents, err := appContentsForExecutable(executable)
	if err != nil {
		return VerifiedNativeRuntime{}, err
	}
	return resolveNativeRuntimeAtContents(contents)
}

func appContentsForExecutable(executable string) (string, error) {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	// Public components are deliberately split across Contents/MacOS,
	// Contents/Helpers, Contents/XPCServices and Resources/runtime. Walk to the
	// containing Contents directory instead of assuming every protected binary
	// is the main application executable.
	for directory := filepath.Dir(executable); ; directory = filepath.Dir(directory) {
		if filepath.Base(directory) == "Contents" {
			info, statErr := os.Lstat(filepath.Join(directory, "Info.plist"))
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("protected Meadow bundle has an invalid Info.plist")
			}
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	return "", fmt.Errorf("protected Meadow binary is not inside an app bundle")
}

func verifyBundle(contents string) error {
	product, err := readVerifiedProductManifest(contents)
	if err != nil {
		return err
	}
	for _, component := range product.Binaries {
		if err := verifyBinary(contents, component); err != nil {
			return err
		}
	}
	return nil
}

func readVerifiedProductManifest(contents string) (productDocument, error) {
	manifestPath := filepath.Join(contents, "Resources", "manifests", "product.json")
	signaturePath := manifestPath + ".sig"
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return productDocument{}, fmt.Errorf("read protected product manifest: %w", err)
	}
	signaturePayload, err := os.ReadFile(signaturePath)
	if err != nil {
		return productDocument{}, fmt.Errorf("read product manifest signature: %w", err)
	}

	var signature signatureDocument
	if err := decodeStrictJSON(signaturePayload, &signature); err != nil {
		return productDocument{}, fmt.Errorf("decode product manifest signature: %w", err)
	}
	// product.json.sig is a separate, deliberately stable snake_case contract.
	// It signs the exact product.json bytes and is not versioned with product v3.
	if signature.SchemaVersion != 1 || signature.Algorithm != "Ed25519" ||
		signature.KeyID == "" || strings.TrimSpace(signature.KeyID) != signature.KeyID ||
		signature.SignatureBase64 == "" {
		return productDocument{}, fmt.Errorf("unsupported product manifest signature contract")
	}
	if expected := strings.TrimSpace(BuildKeyID); expected != "" && signature.KeyID != expected {
		return productDocument{}, fmt.Errorf("product manifest key id mismatch")
	}
	publicKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(BuildPublicKeyB64))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return productDocument{}, fmt.Errorf("protected build has an invalid release public key")
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signature.SignatureBase64)
	if err != nil || len(signatureBytes) != ed25519.SignatureSize {
		return productDocument{}, fmt.Errorf("product manifest signature is malformed")
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signatureBytes) {
		return productDocument{}, fmt.Errorf("product manifest signature verification failed")
	}

	var product productDocument
	if err := decodeStrictJSON(payload, &product); err != nil {
		return productDocument{}, fmt.Errorf("decode protected product manifest: %w", err)
	}
	if err := validateProductDocument(contents, product); err != nil {
		return productDocument{}, err
	}
	return product, nil
}

func decodeStrictJSON(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validateProductDocument(contents string, product productDocument) error {
	if product.SchemaVersion != ProductManifestSchemaVersion {
		return fmt.Errorf("product manifest schemaVersion must be %d", ProductManifestSchemaVersion)
	}
	if err := validateBundleIdentifier(product.BundleID); err != nil {
		return fmt.Errorf("product manifest bundleId is invalid: %w", err)
	}
	if err := validateProductVersion(product.Version); err != nil {
		return fmt.Errorf("product manifest version is invalid: %w", err)
	}
	bundleIdentifier, err := readBundleIdentifier(filepath.Join(contents, "Info.plist"))
	if err != nil {
		return err
	}
	if product.BundleID != bundleIdentifier {
		return fmt.Errorf("signed product manifest bundleId does not match current App Info.plist")
	}
	bundleVersion, err := readBundleVersion(filepath.Join(contents, "Info.plist"))
	if err != nil {
		return err
	}
	if product.Version != bundleVersion {
		return fmt.Errorf("signed product manifest version does not match current App Info.plist")
	}
	if product.Binaries == nil || len(product.Binaries) == 0 {
		return fmt.Errorf("protected product manifest contains no binaries")
	}
	if product.Components == nil {
		return fmt.Errorf("protected product manifest is missing components")
	}

	binariesByPath := make(map[string]binaryDocument, len(product.Binaries))
	binaryPathsByFold := make(map[string]string, len(product.Binaries))
	for index, binary := range product.Binaries {
		label := fmt.Sprintf("product binary %d", index)
		if err := validateBinaryFields(label, binary.Path, binary.Bytes, binary.SHA256, binary.Mode); err != nil {
			return err
		}
		if _, exists := binariesByPath[binary.Path]; exists {
			return fmt.Errorf("product manifest contains duplicate binary path %q", binary.Path)
		}
		folded := strings.ToLower(binary.Path)
		if previous, exists := binaryPathsByFold[folded]; exists {
			return fmt.Errorf("product manifest binary paths have a case-fold collision: %q and %q", previous, binary.Path)
		}
		binariesByPath[binary.Path] = binary
		binaryPathsByFold[folded] = binary.Path
	}

	componentIDsByFold := make(map[string]string, len(product.Components))
	componentPathsByFold := make(map[string]string, len(product.Components))
	componentPaths := make(map[string]struct{}, len(product.Components))
	for index, component := range product.Components {
		label := fmt.Sprintf("product component %d", index)
		if component.ID == "" {
			return fmt.Errorf("%s id and identifier schema fields are required", label)
		}
		if err := validateComponentID(component.ID); err != nil {
			return fmt.Errorf("%s id is invalid: %w", label, err)
		}
		foldedID := strings.ToLower(component.ID)
		if previous, exists := componentIDsByFold[foldedID]; exists {
			if previous == component.ID {
				return fmt.Errorf("product manifest contains duplicate component id %q", component.ID)
			}
			return fmt.Errorf("product manifest component ids have a case-fold collision: %q and %q", previous, component.ID)
		}
		componentIDsByFold[foldedID] = component.ID
		if component.Identifier == "" {
			return fmt.Errorf("%s is missing required identifier field", label)
		}
		if err := validateBundleIdentifier(component.Identifier); err != nil {
			return fmt.Errorf("%s identifier is invalid: %w", label, err)
		}
		if !strings.HasPrefix(component.Identifier, product.BundleID+".") {
			return fmt.Errorf("%s identifier must be namespaced by bundleId %q", label, product.BundleID)
		}
		if err := validateBinaryFields(label, component.Path, component.Bytes, component.SHA256, component.Mode); err != nil {
			return err
		}
		if _, exists := componentPaths[component.Path]; exists {
			return fmt.Errorf("product manifest contains duplicate component path %q", component.Path)
		}
		foldedPath := strings.ToLower(component.Path)
		if previous, exists := componentPathsByFold[foldedPath]; exists {
			return fmt.Errorf("product manifest component paths have a case-fold collision: %q and %q", previous, component.Path)
		}
		componentPaths[component.Path] = struct{}{}
		componentPathsByFold[foldedPath] = component.Path

		binary, exists := binariesByPath[component.Path]
		if !exists {
			if previous, collision := binaryPathsByFold[foldedPath]; collision {
				return fmt.Errorf("product component path %q case-folds to binary path %q but is not an exact reference", component.Path, previous)
			}
			return fmt.Errorf("product component %q does not reference a product binary", component.ID)
		}
		if component.Bytes != binary.Bytes || component.SHA256 != binary.SHA256 || component.Mode != binary.Mode {
			return fmt.Errorf("product component %q metadata does not match binary %q", component.ID, component.Path)
		}
	}
	return nil
}

func validateBinaryFields(label, relative string, size int64, digest string, mode uint32) error {
	if err := validateManifestPath(relative); err != nil {
		return fmt.Errorf("%s path is invalid: %w", label, err)
	}
	if size <= 0 {
		return fmt.Errorf("%s bytes must be positive", label)
	}
	if !isCanonicalSHA256(digest) {
		return fmt.Errorf("%s sha256 must be 64 lowercase hexadecimal characters", label)
	}
	if mode > 0o777 || mode&0o111 == 0 {
		return fmt.Errorf("%s mode must be an executable permission mode", label)
	}
	return nil
}

func validateManifestPath(relative string) error {
	if relative == "" || strings.TrimSpace(relative) != relative || strings.Contains(relative, "\\") || strings.ContainsRune(relative, 0) {
		return fmt.Errorf("path must be a canonical non-empty POSIX relative path")
	}
	if pathpkg.IsAbs(relative) || pathpkg.Clean(relative) != relative {
		return fmt.Errorf("absolute and traversal paths are not allowed")
	}
	for _, segment := range strings.Split(relative, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("empty and traversal path segments are not allowed")
		}
	}
	return nil
}

func validateComponentID(id string) error {
	if id == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("id must be non-empty and canonical")
	}
	for index, character := range id {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return fmt.Errorf("id contains an unsupported character")
	}
	return nil
}

func isCanonicalSHA256(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	for _, character := range digest {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func validateProductVersion(version string) error {
	if strings.TrimSpace(version) != version || !productVersionPattern.MatchString(version) {
		return fmt.Errorf("version must be a canonical semantic version")
	}
	return nil
}

func resolveNativeRuntimeAtContents(contents string) (VerifiedNativeRuntime, error) {
	product, err := readVerifiedProductManifest(contents)
	if err != nil {
		return VerifiedNativeRuntime{}, err
	}
	bundleIdentifier := product.BundleID

	matchIndex := -1
	for index, component := range product.Components {
		if component.ID != NativeRuntimeComponentID {
			continue
		}
		if matchIndex >= 0 {
			return VerifiedNativeRuntime{}, fmt.Errorf("signed product manifest contains duplicate component id %q", NativeRuntimeComponentID)
		}
		matchIndex = index
	}
	if matchIndex < 0 {
		return VerifiedNativeRuntime{}, fmt.Errorf("signed product manifest does not contain component id %q; id and identifier schema fields are required", NativeRuntimeComponentID)
	}
	runtimeComponent := product.Components[matchIndex]
	expectedIdentifier := bundleIdentifier + NativeRuntimeIdentifierSuffix
	if runtimeComponent.Identifier == "" {
		return VerifiedNativeRuntime{}, fmt.Errorf("signed native runtime component is missing required identifier field")
	}
	if runtimeComponent.Identifier != expectedIdentifier {
		return VerifiedNativeRuntime{}, fmt.Errorf("signed native runtime identifier must be %q", expectedIdentifier)
	}
	if runtimeComponent.Path != NativeRuntimeContentsRelativePath {
		return VerifiedNativeRuntime{}, fmt.Errorf("signed native runtime path must be %q", NativeRuntimeContentsRelativePath)
	}

	// Verify every signed binary before returning a component from this product.
	for _, binary := range product.Binaries {
		if err := verifyBinary(contents, binary); err != nil {
			return VerifiedNativeRuntime{}, err
		}
	}
	digestBytes, err := hex.DecodeString(runtimeComponent.SHA256)
	if err != nil || len(digestBytes) != sha256.Size {
		return VerifiedNativeRuntime{}, fmt.Errorf("signed native runtime SHA-256 is malformed")
	}
	var digest [sha256.Size]byte
	copy(digest[:], digestBytes)
	return VerifiedNativeRuntime{
		BundleIdentifier: bundleIdentifier,
		ComponentID: runtimeComponent.ID,
		ComponentIdentifier: runtimeComponent.Identifier,
		ContentsDirectory: contents,
		ContentsRelativePath: runtimeComponent.Path,
		ManifestSize: runtimeComponent.Bytes,
		ManifestSHA256: digest,
	}, nil
}

func readBundleIdentifier(infoPlistPath string) (string, error) {
	value, err := readBundleStringValue(infoPlistPath, "CFBundleIdentifier")
	if err != nil {
		return "", err
	}
	if err := validateBundleIdentifier(value); err != nil {
		return "", fmt.Errorf("current App CFBundleIdentifier is invalid: %w", err)
	}
	return value, nil
}

func readBundleVersion(infoPlistPath string) (string, error) {
	value, err := readBundleStringValue(infoPlistPath, "CFBundleShortVersionString")
	if err != nil {
		return "", err
	}
	if err := validateProductVersion(value); err != nil {
		return "", fmt.Errorf("current App CFBundleShortVersionString is invalid: %w", err)
	}
	return value, nil
}

func readBundleStringValue(infoPlistPath, requestedKey string) (string, error) {
	info, err := os.Lstat(infoPlistPath)
	if err != nil {
		return "", fmt.Errorf("inspect current App Info.plist: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("current App Info.plist is not a direct regular file")
	}
	file, err := os.Open(infoPlistPath)
	if err != nil {
		return "", fmt.Errorf("open current App Info.plist: %w", err)
	}
	defer file.Close()
	decoder := xml.NewDecoder(file)
	waitingForValue := false
	found := false
	value := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("decode current App Info.plist: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			if waitingForValue {
				return "", fmt.Errorf("current App Info.plist has no string value for %s", requestedKey)
			}
			var key string
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return "", fmt.Errorf("decode current App Info.plist key: %w", err)
			}
			waitingForValue = key == requestedKey
			continue
		}
		if !waitingForValue {
			continue
		}
		if start.Name.Local != "string" {
			return "", fmt.Errorf("current App %s is not a string", requestedKey)
		}
		if found {
			return "", fmt.Errorf("current App Info.plist contains duplicate %s", requestedKey)
		}
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return "", fmt.Errorf("decode current App %s: %w", requestedKey, err)
		}
		found = true
		waitingForValue = false
	}
	value = strings.TrimSpace(value)
	if !found || value == "" {
		return "", fmt.Errorf("current App Info.plist is missing %s", requestedKey)
	}
	return value, nil
}

func validateBundleIdentifier(identifier string) error {
	if identifier == "" || strings.HasPrefix(identifier, ".") || strings.HasSuffix(identifier, ".") || strings.Contains(identifier, "..") {
		return fmt.Errorf("invalid bundle identifier structure")
	}
	for _, character := range identifier {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '-' {
			continue
		}
		return fmt.Errorf("unsupported bundle identifier character")
	}
	return nil
}

func verifyBinary(contents string, binary binaryDocument) error {
	target, info, err := directRegularFile(contents, binary.Path)
	if err != nil {
		return err
	}
	if info.Size() != binary.Bytes {
		return fmt.Errorf("protected binary size mismatch: %s", binary.Path)
	}
	if uint32(info.Mode().Perm()) != binary.Mode {
		return fmt.Errorf("protected binary mode mismatch: %s", binary.Path)
	}
	actual, err := sha256File(target)
	if err != nil {
		return fmt.Errorf("hash protected binary %s: %w", binary.Path, err)
	}
	if actual != binary.SHA256 {
		return fmt.Errorf("protected binary integrity verification failed: %s", binary.Path)
	}
	return nil
}

func verifyComponent(contents string, component componentDocument) error {
	return verifyBinary(contents, binaryDocument{
		Path: component.Path, Bytes: component.Bytes, SHA256: component.SHA256, Mode: component.Mode,
	})
}

func directRegularFile(contents, relative string) (string, os.FileInfo, error) {
	contentsInfo, err := os.Lstat(contents)
	if err != nil {
		return "", nil, fmt.Errorf("inspect protected bundle Contents: %w", err)
	}
	if !contentsInfo.IsDir() || contentsInfo.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("protected bundle Contents is not a direct directory")
	}
	current := contents
	segments := strings.Split(relative, "/")
	for index, segment := range segments {
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if err != nil {
			return "", nil, fmt.Errorf("protected binary is missing: %s", relative)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("protected binary path contains a symlink: %s", relative)
		}
		if index < len(segments)-1 {
			if !info.IsDir() {
				return "", nil, fmt.Errorf("protected binary parent is not a directory: %s", relative)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("protected binary is not a regular file: %s", relative)
		}
		return current, info, nil
	}
	return "", nil, fmt.Errorf("protected binary path is empty")
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
