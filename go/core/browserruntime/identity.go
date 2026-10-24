// Package browserruntime checks the local Browser listener against the selected
// packaged executable. Liveness and process ownership are separate: this package
// never starts, terminates, or navigates a Browser.
package browserruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	Unavailable = "browser_unavailable"
	Mismatch    = "browser_runtime_mismatch"
	Unknown     = "browser_runtime_identity_unknown"
)

type identityError struct {
	code   string
	detail string
}

func (e *identityError) Error() string { return e.code + ": " + e.detail }

func NewError(code, detail string) error {
	return &identityError{code: code, detail: detail}
}

func Code(err error) string {
	var identity *identityError
	if errors.As(err, &identity) {
		return identity.code
	}
	return ""
}

func IsUnavailable(err error) bool { return Code(err) == Unavailable }

// IsLocalEndpoint is the transport-protection boundary, independent of whether
// a launcher supports that endpoint's address syntax (for example IPv6 zones).
// Local userinfo is still rejected by Check rather than treated as remote.
func IsLocalEndpoint(base string) bool { return localEndpoint(base) }

// Identity describes the loaded executable vnode and kernel process birth, not
// argv, a PID marker, or the current contents of an executable pathname.
type Identity struct {
	PID        int
	Birth      string
	UID        int
	Device     uint64
	Inode      uint64
	Executable string
}

type Probe func(context.Context, string) (Identity, error)

// CheckBundled intentionally leaves remote transports and unbundled development
// endpoints alone. Explicit MEADOW_BROWSER_BIN selects a binary for this check;
// otherwise only the Browser adjacent to the running packaged executable does.
func CheckBundled(ctx context.Context, base string) error {
	if !localEndpoint(base) {
		return nil
	}
	binary, err := bundledBinary()
	if err != nil {
		return err
	}
	return Check(ctx, base, binary, nil)
}

// RequiresBundledCheck distinguishes an intentionally unbundled endpoint from a
// matching but unhealthy local listener. The latter does not authorize launch.
func RequiresBundledCheck(base string) bool {
	if !localEndpoint(base) {
		return false
	}
	binary, err := bundledBinary()
	return err != nil || binary != ""
}

func bundledBinary() (string, error) {
	binary := strings.TrimSpace(os.Getenv("MEADOW_BROWSER_BIN"))
	if binary == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", NewError(Unknown, "cannot resolve the packaged Browser executable")
		}
		candidate := filepath.Join(filepath.Dir(executable), "..", "Resources", "meadow-browser")
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return "", nil
		} else if err != nil {
			return "", NewError(Unknown, "cannot inspect the packaged Browser executable")
		}
		binary = candidate
	}
	return binary, nil
}

// Check requires a stable listener and the same executable vnode as the selected
// local binary. A replaced file at the same pathname cannot make an old running
// process pass. Separate copies are deliberately not treated as the selected
// packaged artifact, even when their bytes happen to match.
func Check(ctx context.Context, base, expectedBinary string, probe Probe) error {
	if !localEndpoint(base) || strings.TrimSpace(expectedBinary) == "" {
		return nil
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.User != nil {
		return NewError(Unknown, "local Browser endpoint must not contain user information")
	}
	if err := ctx.Err(); err != nil {
		return NewError(Unknown, "Browser identity check was cancelled")
	}
	file, err := os.Open(expectedBinary)
	if err != nil {
		return NewError(Unknown, "cannot open the selected Browser executable")
	}
	defer file.Close()
	want, err := file.Stat()
	if err != nil || !want.Mode().IsRegular() || want.Mode()&0o111 == 0 {
		return NewError(Unknown, "selected Browser executable is not a readable executable file")
	}
	device, inode, err := fileNode(want)
	if err != nil {
		return NewError(Unknown, err.Error())
	}
	if probe == nil {
		probe = nativeProbe
	}
	first, err := probe(ctx, base)
	if err != nil {
		return normalizeProbeError(err)
	}
	if !validIdentity(first) {
		return NewError(Unknown, "listener process identity is incomplete")
	}
	if first.UID != os.Geteuid() {
		return NewError(Mismatch, "Browser listener belongs to another user")
	}
	if first.Device != device || first.Inode != inode {
		return NewError(Mismatch, "running Browser executable differs from the selected packaged artifact; preserve its pages and restart it explicitly")
	}
	second, err := probe(ctx, base)
	if err != nil {
		// Once a listener was observed, its disappearance/replacement is not an
		// absent service that callers may replace automatically.
		return NewError(Unknown, "Browser listener changed during identity verification")
	}
	if !validIdentity(second) || first.PID != second.PID || first.Birth != second.Birth ||
		first.UID != second.UID || first.Device != second.Device || first.Inode != second.Inode {
		return NewError(Unknown, "Browser listener changed during identity verification")
	}
	if ctx.Err() != nil {
		return NewError(Unknown, "Browser identity check was cancelled")
	}
	current, err := os.Stat(expectedBinary)
	if err != nil || !os.SameFile(want, current) || want.Size() != current.Size() || !want.ModTime().Equal(current.ModTime()) {
		return NewError(Unknown, "selected Browser executable changed during identity verification")
	}
	return nil
}

func normalizeProbeError(err error) error {
	if Code(err) != "" {
		return err
	}
	return NewError(Unknown, fmt.Sprintf("cannot verify the local Browser listener: %v", err))
}

func validIdentity(value Identity) bool {
	return value.PID > 1 && value.Birth != "" && value.Inode != 0 && value.Executable != ""
}

func localEndpoint(base string) bool {
	parsed, err := url.Parse(base)
	if err != nil || !strings.EqualFold(parsed.Scheme, "http") || parsed.Host == "" {
		return false
	}
	host, _, _ := strings.Cut(parsed.Hostname(), "%")
	ip := net.ParseIP(host)
	return strings.EqualFold(parsed.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
}
