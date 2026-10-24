package secrets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	bitwardenVersion         = "2.0.0"
	bitwardenDownloadLimit   = 128 << 20
	bitwardenChecksumLimit   = 2 << 20
	defaultBitwardenTokenEnv = "BWS_ACCESS_TOKEN"
)

// BitwardenVersion is the pinned bws release used by native auto-install.
const BitwardenVersion = bitwardenVersion

var bitwardenEnvironmentAllowlist = []string{
	"PATH",
	"HOME",
	"USERPROFILE",
	"APPDATA",
	"LOCALAPPDATA",
	"SystemRoot",
	"TMPDIR",
	"TMP",
	"TEMP",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"NO_PROXY",
	"http_proxy",
	"https_proxy",
	"no_proxy",
}

type bitwardenSource struct{}

var (
	bitwardenMemoryCache = NewMemoryCache()
	bitwardenDiskCache   = DiskCache{BaseName: "bws_cache.json"}
)

func NewBitwardenSource() Source {
	return bitwardenSource{}
}

// LocateBitwarden resolves a managed or PATH bws helper without fetching
// secrets. autoInstall controls whether a missing helper may be installed.
func LocateBitwarden(
	ctx context.Context,
	home string,
	config map[string]any,
	environment Environment,
	autoInstall bool,
) (string, error) {
	return resolveBitwardenBinary(
		ctx,
		FetchRequest{
			Config: config,
			Home:   home,
			Env:    environment,
		},
		autoInstall,
	)
}

// InstallBitwarden downloads and verifies the pinned bws helper. Existing
// managed helpers are reused unless force is true.
func InstallBitwarden(
	ctx context.Context,
	home string,
	force bool,
) (string, error) {
	target := filepath.Join(home, "bin", bitwardenBinaryName())
	if !force && executableFile(target) {
		return target, nil
	}
	return installBitwarden(ctx, home)
}

func (bitwardenSource) Descriptor() Descriptor {
	return Descriptor{
		Name:       "bitwarden",
		Label:      "Bitwarden Secrets Manager",
		Shape:      ShapeBulk,
		Scheme:     "bws",
		APIVersion: APIVersion,
	}
}

func (bitwardenSource) Enabled(config map[string]any) bool {
	return boolConfig(config, "enabled", false)
}

func (bitwardenSource) OverrideExisting(config map[string]any) bool {
	return boolConfig(config, "override_existing", true)
}

func (bitwardenSource) ProtectedEnvVars(config map[string]any) []string {
	return []string{
		stringConfig(
			config,
			"access_token_env",
			defaultBitwardenTokenEnv,
		),
	}
}

func (bitwardenSource) Fetch(
	ctx context.Context,
	request FetchRequest,
) FetchResult {
	result := FetchResult{Secrets: map[string]string{}}
	tokenEnv := stringConfig(
		request.Config,
		"access_token_env",
		defaultBitwardenTokenEnv,
	)
	token, exists := request.Env.Lookup(tokenEnv)
	token = strings.TrimSpace(token)
	if !exists || token == "" {
		result.Error = fmt.Sprintf(
			"secrets.bitwarden.enabled is true but %s is not set",
			tokenEnv,
		)
		result.ErrorKind = ErrorNotConfigured
		return result
	}
	projectID := stringConfig(request.Config, "project_id", "")
	if projectID == "" {
		result.Error = "secrets.bitwarden.project_id is empty"
		result.ErrorKind = ErrorNotConfigured
		return result
	}

	binary, err := resolveBitwardenBinary(
		ctx,
		request,
		boolConfig(request.Config, "auto_install", true),
	)
	if err != nil {
		result.Error = scrubDiagnostic(err.Error())
		result.ErrorKind = ErrorBinaryMissing
		if errors.Is(err, context.DeadlineExceeded) {
			result.ErrorKind = ErrorTimeout
		}
		return result
	}
	result.BinaryPath = binary

	ttl := floatConfig(request.Config, "cache_ttl_seconds", 300)
	useCache := boolConfig(request.Config, "use_cache", true) && ttl > 0
	cacheKey := strings.Join(
		[]string{
			fingerprint(token),
			projectID,
			stringConfig(request.Config, "server_url", ""),
		},
		"|",
	)
	now := time.Now()
	if useCache {
		if cached, found := bitwardenMemoryCache.Get(cacheKey, ttl, now); found {
			result.Secrets = cached.Secrets
			return result
		}
		if cached, found := bitwardenDiskCache.Read(
			cacheKey,
			ttl,
			request.Home,
			now,
		); found {
			bitwardenMemoryCache.Put(cacheKey, cached)
			result.Secrets = cached.Secrets
			return result
		}
	}

	childEnvironment := minimalEnvironment(
		request.Env,
		bitwardenEnvironmentAllowlist,
	)
	childEnvironment[defaultBitwardenTokenEnv] = token
	if serverURL := stringConfig(request.Config, "server_url", ""); serverURL != "" {
		childEnvironment["BWS_SERVER_URL"] = serverURL
	} else if inherited, ok := request.Env.Lookup("BWS_SERVER_URL"); ok {
		childEnvironment["BWS_SERVER_URL"] = inherited
	}

	command := runCommand(
		ctx,
		binary,
		[]string{"secret", "list", projectID, "--output", "json"},
		childEnvironment,
	)
	if command.err != nil {
		result.Error, result.ErrorKind = commandError("bws", command)
		result.Error = redactSensitiveEnvironment(
			result.Error,
			childEnvironment,
		)
		return result
	}
	raw := strings.TrimSpace(command.stdout)
	if raw == "" {
		result.Warnings = append(
			result.Warnings,
			"Bitwarden returned no output (empty project?)",
		)
		cacheBitwardenResult(
			useCache,
			cacheKey,
			ttl,
			request.Home,
			result.Secrets,
		)
		return result
	}
	var payload []struct {
		Key   any `json:"key"`
		Value any `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		result.Error = "bws returned non-JSON or unexpected output"
		result.ErrorKind = ErrorInternal
		return result
	}
	for _, item := range payload {
		key, keyOK := item.Key.(string)
		value, valueOK := item.Value.(string)
		if !keyOK || !valueOK {
			continue
		}
		if !validEnvName(key) {
			result.Warnings = append(
				result.Warnings,
				fmt.Sprintf(
					"Bitwarden skipped %q: invalid environment name",
					key,
				),
			)
			continue
		}
		result.Secrets[key] = value
	}
	cacheBitwardenResult(
		useCache,
		cacheKey,
		ttl,
		request.Home,
		result.Secrets,
	)
	return result
}

func cacheBitwardenResult(
	enabled bool,
	key string,
	ttl float64,
	home string,
	values map[string]string,
) {
	if !enabled {
		return
	}
	entry := CachedFetch{
		Secrets:   values,
		FetchedAt: float64(time.Now().UnixNano()) / 1e9,
	}
	bitwardenMemoryCache.Put(key, entry)
	_ = bitwardenDiskCache.Write(key, entry, ttl, home)
}

func resetBitwardenCache(home string) {
	bitwardenMemoryCache.Clear()
	_ = bitwardenDiskCache.Clear(home)
}

func resolveBitwardenBinary(
	ctx context.Context,
	request FetchRequest,
	autoInstall bool,
) (string, error) {
	if pinned := stringConfig(request.Config, "binary_path", ""); pinned != "" {
		return findExecutable(pinned, "bws", request.Env)
	}
	name := bitwardenBinaryName()
	managed := filepath.Join(request.Home, "bin", name)
	if executableFile(managed) {
		return managed, nil
	}
	binary, err := findExecutable("", name, request.Env)
	if err == nil {
		return binary, nil
	}
	if !autoInstall {
		return "", fmt.Errorf(
			"bws binary is unavailable and auto_install is disabled",
		)
	}
	return installBitwarden(ctx, request.Home)
}

func bitwardenBinaryName() string {
	if runtime.GOOS == "windows" {
		return "bws.exe"
	}
	return "bws"
}

func bitwardenAssetName(ctx context.Context) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf(
			"bws-macos-universal-%s.zip",
			bitwardenVersion,
		), nil
	case "windows":
		architecture := "x86_64"
		if runtime.GOARCH == "arm64" {
			architecture = "aarch64"
		}
		return fmt.Sprintf(
			"bws-%s-pc-windows-msvc-%s.zip",
			architecture,
			bitwardenVersion,
		), nil
	case "linux":
		architecture := "x86_64"
		if runtime.GOARCH == "arm64" {
			architecture = "aarch64"
		}
		libc := "gnu"
		command := exec.CommandContext(ctx, "ldd", "--version")
		output, err := command.CombinedOutput()
		if err == nil && strings.Contains(strings.ToLower(string(output)), "musl") {
			libc = "musl"
		}
		return fmt.Sprintf(
			"bws-%s-unknown-linux-%s-%s.zip",
			architecture,
			libc,
			bitwardenVersion,
		), nil
	default:
		return "", fmt.Errorf(
			"unsupported platform for bws auto-install: %s/%s",
			runtime.GOOS,
			runtime.GOARCH,
		)
	}
}

func installBitwarden(ctx context.Context, home string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("cannot auto-install bws without a Meadow home")
	}
	asset, err := bitwardenAssetName(ctx)
	if err != nil {
		return "", err
	}
	baseURL := fmt.Sprintf(
		"https://github.com/bitwarden/sdk-sm/releases/download/bws-v%s",
		bitwardenVersion,
	)
	checksumName := fmt.Sprintf(
		"bws-sha256-checksums-%s.txt",
		bitwardenVersion,
	)
	client := &http.Client{Timeout: 60 * time.Second}
	archive, err := downloadLimited(
		ctx,
		client,
		baseURL+"/"+asset,
		bitwardenDownloadLimit,
	)
	if err != nil {
		return "", fmt.Errorf("download bws archive: %w", err)
	}
	checksums, err := downloadLimited(
		ctx,
		client,
		baseURL+"/"+checksumName,
		bitwardenChecksumLimit,
	)
	if err != nil {
		return "", fmt.Errorf("download bws checksums: %w", err)
	}
	expected, err := expectedChecksum(string(checksums), asset)
	if err != nil {
		return "", err
	}
	actualBytes := sha256.Sum256(archive)
	actual := hex.EncodeToString(actualBytes[:])
	if !strings.EqualFold(actual, expected) {
		return "", fmt.Errorf(
			"checksum mismatch for %s: expected %s, got %s",
			asset,
			expected,
			actual,
		)
	}

	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return "", fmt.Errorf("open bws archive: %w", err)
	}
	member, err := selectBinaryMember(reader, bitwardenBinaryName())
	if err != nil {
		return "", err
	}
	source, err := member.Open()
	if err != nil {
		return "", fmt.Errorf("open bws archive member: %w", err)
	}
	defer source.Close()

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(binDir, bitwardenBinaryName())
	temp, err := os.CreateTemp(binDir, ".bws-*")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.Copy(temp, io.LimitReader(source, bitwardenDownloadLimit)); err != nil {
		return "", err
	}
	if err := temp.Chmod(0o755); err != nil {
		return "", err
	}
	if err := temp.Sync(); err != nil {
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tempPath, target); err != nil {
		return "", err
	}
	keep = true
	return target, nil
}

func downloadLimited(
	ctx context.Context,
	client *http.Client,
	url string,
	limit int64,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "meadow-cli")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("download exceeded %d bytes", limit)
	}
	return content, nil
}

func expectedChecksum(content, asset string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == asset {
			if len(fields[0]) != sha256.Size*2 {
				return "", fmt.Errorf("invalid checksum for %s", asset)
			}
			if _, err := hex.DecodeString(fields[0]); err != nil {
				return "", fmt.Errorf("invalid checksum for %s", asset)
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum entry for %s was not found", asset)
}

func selectBinaryMember(
	reader *zip.Reader,
	binaryName string,
) (*zip.File, error) {
	var selected *zip.File
	for _, member := range reader.File {
		clean := filepath.ToSlash(filepath.Clean(member.Name))
		if strings.HasPrefix(clean, "../") ||
			strings.HasPrefix(clean, "/") ||
			strings.Contains(clean, "/../") {
			return nil, fmt.Errorf(
				"refusing unsafe bws archive member %q",
				member.Name,
			)
		}
		if filepath.Base(clean) != binaryName || member.FileInfo().IsDir() {
			continue
		}
		if selected == nil || len(clean) < len(selected.Name) {
			selected = member
		}
	}
	if selected == nil {
		return nil, fmt.Errorf(
			"bws archive does not contain %s",
			binaryName,
		)
	}
	return selected, nil
}
