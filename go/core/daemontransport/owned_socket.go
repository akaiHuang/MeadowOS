package daemontransport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/localipc"
	"github.com/hey-meadow/meadow-code/go/core/portablepaths"
)

// This receipt exists only after readiness proves the socket peer is our exact
// child. It is never reconstructed from a stale socket or an advertised PID.
// The directory handle prevents ancestor replacement from redirecting Remove.
type ownedReadySocket struct {
	profilePath string
	profileInfo os.FileInfo
	dirPath     string
	dirInfo     os.FileInfo
	path        string
	info        os.FileInfo
	directory   *os.Root
}

func (supervisor *Supervisor) captureReadySocket(ctx context.Context, fingerprint string, config LaunchConfig) error {
	if supervisor == nil || !supervisor.owned || supervisor.command == nil || supervisor.command.Process == nil {
		return fmt.Errorf("socket capture requires an owned child")
	}
	root := strings.TrimSpace(config.StateRoot)
	if root == "" {
		root = os.Getenv("MEADOW_HOME")
	}
	profilePath, err := portablepaths.StateRoot(root, os.UserConfigDir)
	if err != nil {
		return err
	}
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(socketPath) || socketPath != filepath.Join(profilePath, "run", "agent.sock") {
		return fmt.Errorf("owned socket is outside the selected private profile")
	}
	profileInfo, err := os.Lstat(profilePath)
	if err != nil || !privateOwnedDirectory(profileInfo) {
		return fmt.Errorf("owned socket profile is not a private current-user directory")
	}
	profile, err := os.OpenRoot(profilePath)
	if err != nil {
		return err
	}
	defer profile.Close()
	if info, err := profile.Stat("."); err != nil || !samePrivateDirectory(profileInfo, info) {
		return fmt.Errorf("owned socket profile changed while opening")
	}
	dirInfo, err := profile.Lstat("run")
	if err != nil || !privateOwnedDirectory(dirInfo) {
		return fmt.Errorf("owned socket parent is not a private current-user directory")
	}
	directory, err := profile.OpenRoot("run")
	if err != nil {
		return err
	}
	retained := false
	defer func() {
		if !retained {
			_ = directory.Close()
		}
	}()
	if info, err := directory.Stat("."); err != nil || !samePrivateDirectory(dirInfo, info) {
		return fmt.Errorf("owned socket parent changed while opening")
	}
	info, err := directory.Lstat("agent.sock")
	if err != nil || !privateOwnedSocket(info) {
		return fmt.Errorf("owned socket is not a private current-user Unix socket")
	}
	receipt := &ownedReadySocket{
		profilePath: profilePath, profileInfo: profileInfo,
		dirPath: filepath.Dir(socketPath), dirInfo: dirInfo,
		path: socketPath, info: info, directory: directory,
	}
	if err := receipt.validate(); err != nil {
		return err
	}
	token, err := localipc.ExistingAgentToken()
	if err != nil {
		return err
	}
	probeContext, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	identity, err := probeDaemonWithClient(probeContext, newClientAtSocket(socketPath, token), "/transport/identity", true, peerPIDFromConn)
	if err != nil {
		return fmt.Errorf("verify owned socket peer: %w", err)
	}
	if !daemonReadinessMatchesLaunch(identity, fingerprint, config, supervisor.command.Process.Pid) {
		return fmt.Errorf("owned socket identity differs from the ready child")
	}
	if err := receipt.validate(); err != nil {
		return err
	}
	supervisor.mu.Lock()
	if supervisor.readySocket != nil {
		supervisor.mu.Unlock()
		return fmt.Errorf("owned socket was already captured")
	}
	supervisor.readySocket = receipt
	reaped := supervisor.reaped
	retained = true
	supervisor.mu.Unlock()
	if reaped {
		_ = supervisor.cleanupReapedSocket()
		return fmt.Errorf("owned child exited while capturing its socket")
	}
	return nil
}

// A successful Wait is required even if the socket has already disappeared.
// Reused helpers and children that never reached authenticated readiness cannot
// acquire a receipt, so neither is eligible for path cleanup.
func (supervisor *Supervisor) cleanupReapedSocket() error {
	if supervisor == nil || !supervisor.owned {
		return nil
	}
	supervisor.mu.Lock()
	receipt, reaped := supervisor.readySocket, supervisor.reaped
	supervisor.mu.Unlock()
	if receipt == nil || !reaped {
		return nil
	}
	supervisor.socketCleanup.Do(func() {
		defer receipt.directory.Close()
		if err := receipt.validateDirectories(); err != nil {
			supervisor.socketError = err
			return
		}
		info, err := receipt.directory.Lstat("agent.sock")
		if os.IsNotExist(err) {
			return // A graceful listener close already removed its own socket.
		}
		if err != nil || !samePrivateSocket(receipt.info, info) {
			supervisor.socketError = fmt.Errorf("owned Agent socket changed; preserving the current object")
			return
		}
		// Unix has no atomic compare-inode-and-unlink operation. The private
		// parent, pinned directory handle and final identity check bound cleanup
		// to our known socket; this is not a general stale-socket recovery path.
		// A malicious same-UID basename swap after this check is outside this
		// guarantee; cooperating Meadow listeners reject the occupied path.
		if err := receipt.directory.Remove("agent.sock"); err != nil && !os.IsNotExist(err) {
			supervisor.socketError = fmt.Errorf("remove reaped owned Agent socket: %w", err)
		}
	})
	return supervisor.socketError
}

func (receipt *ownedReadySocket) validateDirectories() error {
	profile, err := os.Lstat(receipt.profilePath)
	if err != nil || !samePrivateDirectory(receipt.profileInfo, profile) {
		return fmt.Errorf("owned Agent profile changed; preserving its socket")
	}
	directory, err := os.Lstat(receipt.dirPath)
	if err != nil || !samePrivateDirectory(receipt.dirInfo, directory) {
		return fmt.Errorf("owned Agent socket directory changed; preserving its socket")
	}
	anchored, err := receipt.directory.Stat(".")
	if err != nil || !samePrivateDirectory(receipt.dirInfo, anchored) {
		return fmt.Errorf("owned Agent socket directory handle changed")
	}
	return nil
}

func (receipt *ownedReadySocket) validate() error {
	if err := receipt.validateDirectories(); err != nil {
		return err
	}
	info, err := os.Lstat(receipt.path)
	if err != nil || !samePrivateSocket(receipt.info, info) {
		return fmt.Errorf("owned Agent socket changed during capture")
	}
	anchored, err := receipt.directory.Lstat("agent.sock")
	if err != nil || !samePrivateSocket(receipt.info, anchored) {
		return fmt.Errorf("owned Agent socket handle differs during capture")
	}
	return nil
}

func privateOwnedDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && fileOwnedByCurrentUser(info)
}

func privateOwnedSocket(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSocket != 0 && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && fileOwnedByCurrentUser(info)
}

func samePrivateDirectory(saved, current os.FileInfo) bool {
	return privateOwnedDirectory(saved) && privateOwnedDirectory(current) && os.SameFile(saved, current) && saved.Mode() == current.Mode()
}

func samePrivateSocket(saved, current os.FileInfo) bool {
	return privateOwnedSocket(saved) && privateOwnedSocket(current) && os.SameFile(saved, current) && saved.Mode() == current.Mode() && saved.ModTime().Equal(current.ModTime())
}
