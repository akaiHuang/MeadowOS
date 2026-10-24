package productupdate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const StagedArchiveFileName = "payload.pkg"

var (
	ErrStateNotFound = errors.New("product update state not found")
	ErrCASConflict   = errors.New("product update state compare-and-swap conflict")
	stagedIDPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	teamIDPattern    = regexp.MustCompile(`^[A-Z0-9]{10}$`)
)

// BuildConfig is immutable product configuration supplied by the signed build
// composition. This package never reads environment variables, settings, or
// request parameters to populate it.
type BuildConfig struct {
	Product                string
	Channel                string
	FeedID                 string
	FeedOrigin             string
	FeedPath               string
	KeyringPathTemplate    string
	ArchivePathTemplate    string
	Platform               string
	Architecture           string
	BundleIdentifier       string
	DesignatedRequirement  string
	CurrentOS              string
	CurrentVersion         string
	CurrentBuildNumber     int64
	MinimumFeedSequence    int64
	InstallerTeamID        string
	StagingRoot            string
	BootstrapKeyringJSON   []byte
	BootstrapKeyringSHA256 string
	MaxDocumentBytes       int64
	MaxArchiveBytes        int64
}

// Download is transport evidence returned by a fixed-origin client. A response
// is rejected unless it is a complete 200 response with no redirect and both
// origins exactly equal the build-time origin.
type Download struct {
	Body          io.ReadCloser
	StatusCode    int
	ContentLength int64
	Origin        string
	FinalOrigin   string
	Redirected    bool
}

// FixedFeedClient exposes no URL-bearing method. Implementations are composed
// with one build-time feed origin and must disable redirects.
type FixedFeedClient interface {
	FetchFeed(context.Context) (Download, error)
	FetchKeyring(context.Context, int64) (Download, error)
	FetchArchive(context.Context, string) (Download, error)
}

type PendingUpdate struct {
	StagedID         string
	Status           string
	Product          string
	FeedID           string
	FeedSequence     int64
	FeedDigest       string
	ReleaseID        string
	Version          string
	BuildNumber      int64
	KeyringSequence  int64
	Artifact         Artifact
	StagedAt         string
	HandoffAt        string
	HandoffReference string
}

// ProductState wraps the canonical state with the currently pinned keyring and
// a not-yet-authoritative staged/handoff record.
type ProductState struct {
	CurrentKeyring Keyring
	Accepted       ContractState
	Pending        *PendingUpdate
}

type StoredProductState struct {
	Revision string
	State    ProductState
}

// UpdateStateStore is the sole authority for rollback/replay and pending state.
// Its implementation and durable storage policy are intentionally out of scope.
type UpdateStateStore interface {
	Load(context.Context) (StoredProductState, error)
	CompareAndSwap(context.Context, string, ProductState) (StoredProductState, error)
}

type HandoffReceipt struct {
	Reference string
}

// SystemInstaller transfers an already-verified package to the privileged
// platform installer. A successful handoff is not installation acceptance.
type SystemInstaller interface {
	Handoff(context.Context, string) (HandoffReceipt, error)
}

type Installer interface {
	VerifyAndHandoff(context.Context, string, Artifact, string) (HandoffReceipt, error)
}

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type InstalledProductEvidence struct {
	Product           string
	Version           string
	BuildNumber       int64
	IntegrityVerified bool
	IntegrityDigest   string
}

type PendingProductEvidence struct {
	Product        string
	Version        string
	BuildNumber    int64
	StagedID       string
	ArtifactSize   int64
	ArtifactSHA256 string
}

// ProductIntegrityVerifier examines the installed bundle, not caller-provided
// request fields. Success is committed only when its evidence matches pending.
type ProductIntegrityVerifier interface {
	VerifyInstalledProduct(context.Context, PendingProductEvidence) (InstalledProductEvidence, error)
}

type Dependencies struct {
	Client          FixedFeedClient
	Store           UpdateStateStore
	Installer       Installer
	ProductVerifier ProductIntegrityVerifier
	Clock           func() time.Time
	Random          io.Reader
}

type Service struct {
	config    BuildConfig
	bootstrap Keyring
	client    FixedFeedClient
	store     UpdateStateStore
	installer Installer
	verifier  ProductIntegrityVerifier
	clock     func() time.Time
	random    io.Reader
}

type CheckResult struct {
	FeedID       string
	Sequence     int64
	ReleaseID    string
	Version      string
	BuildNumber  int64
	FeedDigest   string
	ArtifactID   string
	Architecture string
	MinOS        string
	StagedID     string
}

type StageResult struct {
	StagedID    string
	ReleaseID   string
	Version     string
	BuildNumber int64
}

type InstallResult struct {
	StagedID         string
	HandoffReference string
	Accepted         bool
}

type CommitResult struct {
	FeedID      string
	Sequence    int64
	Version     string
	BuildNumber int64
	Accepted    bool
}

// InstallRecoveryResult describes the startup-only recovery of a durable
// install state. Retryable never means accepted: it only means a previous
// system-installer handoff did not produce a verifiable installed product and
// may be handed off again after a new, explicit install request.
type InstallRecoveryResult struct {
	StagedID  string
	Accepted  bool
	Retryable bool
}

type candidate struct {
	feed       Feed
	keyring    Keyring
	nextState  ProductState
	feedDigest string
	artifact   Artifact
	rotated    bool
}

func NewService(config BuildConfig, dependencies Dependencies) (*Service, error) {
	if dependencies.Client == nil || dependencies.Store == nil || dependencies.Installer == nil || dependencies.ProductVerifier == nil {
		return nil, contractError("CONFIG_DEPENDENCY", "client, store, installer, and product verifier are required", nil)
	}
	if dependencies.Clock == nil {
		dependencies.Clock = time.Now
	}
	if dependencies.Random == nil {
		dependencies.Random = rand.Reader
	}
	if config.MaxDocumentBytes == 0 {
		config.MaxDocumentBytes = 4 << 20
	}
	if config.MaxArchiveBytes == 0 {
		config.MaxArchiveBytes = 16 << 30
	}
	if err := validateBuildConfig(config); err != nil {
		return nil, err
	}
	bootstrap, err := ParseKeyring(config.BootstrapKeyringJSON)
	if err != nil {
		return nil, err
	}
	digest, err := DocumentDigest(bootstrap)
	if err != nil {
		return nil, err
	}
	if digest != config.BootstrapKeyringSHA256 {
		return nil, contractError("BOOTSTRAP_KEYRING_DIGEST", "bootstrap keyring digest mismatch", nil)
	}
	if err := verifyBootstrapKeyring(bootstrap, dependencies.Clock().UTC()); err != nil {
		return nil, err
	}
	config.BootstrapKeyringJSON = append([]byte(nil), config.BootstrapKeyringJSON...)
	return &Service{
		config: config, bootstrap: bootstrap, client: dependencies.Client,
		store: dependencies.Store, installer: dependencies.Installer,
		verifier: dependencies.ProductVerifier, clock: dependencies.Clock,
		random: dependencies.Random,
	}, nil
}

func validateBuildConfig(config BuildConfig) error {
	if config.Product == "" || config.Channel == "" {
		return contractError("CONFIG_IDENTITY", "product and channel are required", nil)
	}
	if err := validateID(config.FeedID, "CONFIG_FEED_ID", "feedId"); err != nil {
		return err
	}
	origin, err := url.Parse(config.FeedOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" {
		return contractError("CONFIG_FEED_ORIGIN", "feed origin must be an HTTPS origin without path, credentials, query, or fragment", err)
	}
	if origin.String() != config.FeedOrigin && strings.TrimSuffix(origin.String(), "/") != config.FeedOrigin {
		return contractError("CONFIG_FEED_ORIGIN", "feed origin is not canonical", nil)
	}
	if config.Platform != "macos" && config.Platform != "linux" && config.Platform != "windows" {
		return contractError("CONFIG_PLATFORM", "unsupported product platform", nil)
	}
	if config.Architecture != "arm64" && config.Architecture != "x64" {
		return contractError("CONFIG_ARCHITECTURE", "unsupported product architecture", nil)
	}
	if err := validateFixedEndpointPath(config.FeedPath, ""); err != nil {
		return err
	}
	if err := validateKeyringEndpointPath(config.KeyringPathTemplate); err != nil {
		return err
	}
	if err := validateFixedEndpointPath(config.ArchivePathTemplate, artifactIDPlaceholder); err != nil {
		return err
	}
	if !idPattern.MatchString(config.BundleIdentifier) || strings.TrimSpace(config.DesignatedRequirement) == "" || strings.ContainsAny(config.DesignatedRequirement, "\r\n\x00") {
		return contractError("CONFIG_BUNDLE_IDENTITY", "bundle identifier and designated requirement are invalid", nil)
	}
	if !osVersionPattern.MatchString(config.CurrentOS) {
		return contractError("CONFIG_OS", "current OS version is invalid", nil)
	}
	if err := validateVersion(config.CurrentVersion); err != nil {
		return err
	}
	if err := requirePositiveSafe(config.CurrentBuildNumber, "CONFIG_BUILD", "current build number"); err != nil {
		return err
	}
	if config.MinimumFeedSequence < 0 || config.MinimumFeedSequence > maxSafeInteger {
		return contractError("CONFIG_FEED_SEQUENCE", "minimum feed sequence is invalid", nil)
	}
	if config.Platform == "macos" && !teamIDPattern.MatchString(config.InstallerTeamID) {
		return contractError("CONFIG_TEAM_ID", "Installer Team ID must be ten uppercase alphanumeric characters", nil)
	}
	if !filepath.IsAbs(config.StagingRoot) || filepath.Clean(config.StagingRoot) != config.StagingRoot {
		return contractError("CONFIG_STAGING_ROOT", "staging root must be an absolute clean path", nil)
	}
	if !digestPattern.MatchString(config.BootstrapKeyringSHA256) {
		return contractError("CONFIG_BOOTSTRAP_DIGEST", "bootstrap keyring digest is invalid", nil)
	}
	if config.MaxDocumentBytes <= 0 || config.MaxDocumentBytes > maxSafeInteger || config.MaxArchiveBytes <= 0 || config.MaxArchiveBytes > maxSafeInteger {
		return contractError("CONFIG_LIMIT", "download limits must be positive safe integers", nil)
	}
	return nil
}

func (service *Service) Check(ctx context.Context) (CheckResult, error) {
	stored, err := service.loadOrCreateState(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	resolved, err := service.resolveCandidate(ctx, stored)
	if err != nil {
		return CheckResult{}, err
	}
	if resolved.rotated {
		if _, err := service.store.CompareAndSwap(ctx, stored.Revision, resolved.nextState); err != nil {
			return CheckResult{}, contractError("STATE_CONFLICT", "keyring rotation state changed concurrently", err)
		}
	}
	result := checkResult(resolved)
	if resolved.nextState.Pending != nil && resolved.nextState.Pending.FeedDigest == resolved.feedDigest {
		result.StagedID = resolved.nextState.Pending.StagedID
	}
	return result, nil
}

func checkResult(resolved candidate) CheckResult {
	return CheckResult{
		FeedID: resolved.feed.FeedID, Sequence: resolved.feed.Sequence,
		ReleaseID: resolved.feed.Release.ReleaseID, Version: resolved.feed.Release.Version,
		BuildNumber: resolved.feed.Release.BuildNumber, FeedDigest: resolved.feedDigest,
		ArtifactID: resolved.artifact.ArtifactID, Architecture: resolved.artifact.Architecture,
		MinOS: resolved.artifact.MinOS,
	}
}

func (service *Service) Stage(ctx context.Context) (StageResult, error) {
	stored, err := service.loadOrCreateState(ctx)
	if err != nil {
		return StageResult{}, err
	}
	resolved, err := service.resolveCandidate(ctx, stored)
	if err != nil {
		return StageResult{}, err
	}
	if pending := resolved.nextState.Pending; pending != nil && pending.FeedDigest == resolved.feedDigest && pending.Status != "" {
		if err := service.verifyStagedArchive(pending); err == nil {
			return StageResult{StagedID: pending.StagedID, ReleaseID: pending.ReleaseID, Version: pending.Version, BuildNumber: pending.BuildNumber}, nil
		}
	}
	stagedID, err := service.newStagedID()
	if err != nil {
		return StageResult{}, err
	}
	if err := service.stageArchive(ctx, stagedID, resolved.artifact); err != nil {
		return StageResult{}, err
	}
	now := canonicalNow(service.clock().UTC())
	next := resolved.nextState
	previous := next.Pending
	next.Pending = &PendingUpdate{
		StagedID: stagedID, Status: "staged", Product: resolved.feed.Product, FeedID: resolved.feed.FeedID,
		FeedSequence: resolved.feed.Sequence, FeedDigest: resolved.feedDigest,
		ReleaseID: resolved.feed.Release.ReleaseID, Version: resolved.feed.Release.Version,
		BuildNumber: resolved.feed.Release.BuildNumber, KeyringSequence: resolved.keyring.Sequence,
		Artifact: resolved.artifact, StagedAt: now,
	}
	if err := validateProductState(next); err != nil {
		service.removeStage(stagedID)
		return StageResult{}, err
	}
	if _, err := service.store.CompareAndSwap(ctx, stored.Revision, next); err != nil {
		service.removeStage(stagedID)
		return StageResult{}, contractError("STATE_CONFLICT", "staged state changed concurrently", err)
	}
	if previous != nil && previous.StagedID != stagedID {
		service.removeStage(previous.StagedID)
	}
	return StageResult{StagedID: stagedID, ReleaseID: next.Pending.ReleaseID, Version: next.Pending.Version, BuildNumber: next.Pending.BuildNumber}, nil
}

func (service *Service) Install(ctx context.Context, stagedID string) (InstallResult, error) {
	if !stagedIDPattern.MatchString(stagedID) {
		return InstallResult{}, contractError("STAGED_ID", "stagedId is not an opaque product-update identifier", nil)
	}
	stored, err := service.store.Load(ctx)
	if err != nil {
		return InstallResult{}, contractError("STATE_LOAD", "cannot load authoritative update state", err)
	}
	if err := validateProductState(stored.State); err != nil {
		return InstallResult{}, err
	}
	pending := stored.State.Pending
	if pending == nil || pending.StagedID != stagedID {
		return InstallResult{}, contractError("STAGED_ID", "stagedId does not identify the authoritative pending update", nil)
	}
	if pending.Status == "handoff" {
		return InstallResult{StagedID: stagedID, HandoffReference: pending.HandoffReference, Accepted: false}, nil
	}
	if pending.Status != "staged" && pending.Status != "retryable" {
		return InstallResult{}, contractError("INSTALL_IN_PROGRESS", "pending update is not available for a new handoff", nil)
	}
	claimed := stored.State
	claimed.Pending = clonePending(pending)
	claimed.Pending.Status = "installing"
	claimed.Pending.HandoffAt = ""
	claimed.Pending.HandoffReference = ""
	if err := validateProductState(claimed); err != nil {
		return InstallResult{}, err
	}
	claimedStored, err := service.store.CompareAndSwap(ctx, stored.Revision, claimed)
	if err != nil {
		return InstallResult{}, contractError("STATE_CONFLICT", "install claim changed concurrently", err)
	}
	archivePath, err := service.stagedArchivePath(pending)
	if err == nil {
		err = service.verifyStagedArchive(pending)
	}
	var receipt HandoffReceipt
	if err == nil {
		receipt, err = service.installer.VerifyAndHandoff(ctx, archivePath, pending.Artifact, service.config.InstallerTeamID)
		if err == nil && receipt.Reference == "" {
			err = contractError("INSTALL_HANDOFF", "system installer returned an empty handoff reference", nil)
		}
	}
	if err != nil {
		if rollbackErr := service.restoreInstallClaim(ctx, claimedStored, pending); rollbackErr != nil {
			return InstallResult{}, contractError("STATE_CONFLICT", "failed install claim could not be restored", errors.Join(err, rollbackErr))
		}
		return InstallResult{}, err
	}
	next := claimedStored.State
	if next.Pending == nil || next.Pending.StagedID != stagedID || next.Pending.Status != "installing" {
		return InstallResult{}, contractError("STATE_CONFLICT", "install claim was replaced", nil)
	}
	next.Pending = clonePending(next.Pending)
	next.Pending.Status = "handoff"
	next.Pending.HandoffAt = canonicalNow(service.clock().UTC())
	next.Pending.HandoffReference = receipt.Reference
	if _, err := service.store.CompareAndSwap(ctx, claimedStored.Revision, next); err != nil {
		reference, reconcileErr := service.reconcileHandoffState(ctx, pending, receipt, err)
		if reconcileErr != nil {
			return InstallResult{}, reconcileErr
		}
		return InstallResult{StagedID: stagedID, HandoffReference: reference, Accepted: false}, nil
	}
	return InstallResult{StagedID: stagedID, HandoffReference: receipt.Reference, Accepted: false}, nil
}

func (service *Service) restoreInstallClaim(ctx context.Context, claimed StoredProductState, previous *PendingUpdate) error {
	rollback := claimed.State
	if rollback.Pending == nil || rollback.Pending.StagedID != previous.StagedID || rollback.Pending.Status != "installing" {
		return contractError("STATE_CONFLICT", "install claim was replaced before rollback", nil)
	}
	rollback.Pending = clonePending(previous)
	if _, err := service.store.CompareAndSwap(ctx, claimed.Revision, rollback); err == nil {
		return nil
	}

	latest, err := service.store.Load(ctx)
	if err != nil {
		return contractError("STATE_LOAD", "cannot reconcile failed install claim", err)
	}
	if err := validateProductState(latest.State); err != nil {
		return err
	}
	if pendingWasAccepted(latest.State, previous) {
		return nil
	}
	pending := latest.State.Pending
	if pending == nil || pending.StagedID != previous.StagedID {
		return contractError("STATE_CONFLICT", "failed install claim was replaced", nil)
	}
	if pending.Status == previous.Status || pending.Status == "handoff" || pending.Status == "retryable" {
		return nil
	}
	if pending.Status != "installing" {
		return contractError("STATE_CONFLICT", "failed install claim has an unexpected status", nil)
	}
	retry := latest.State
	retry.Pending = clonePending(previous)
	if _, err := service.store.CompareAndSwap(ctx, latest.Revision, retry); err != nil {
		return contractError("STATE_CONFLICT", "failed install claim rollback changed concurrently", err)
	}
	return nil
}

func (service *Service) reconcileHandoffState(ctx context.Context, previous *PendingUpdate, receipt HandoffReceipt, originalErr error) (string, error) {
	latest, err := service.store.Load(ctx)
	if err != nil {
		return "", contractError("STATE_LOAD", "cannot reconcile system-installer handoff", errors.Join(originalErr, err))
	}
	if err := validateProductState(latest.State); err != nil {
		return "", err
	}
	if pendingWasAccepted(latest.State, previous) {
		service.removeStage(previous.StagedID)
		return receipt.Reference, nil
	}
	pending := latest.State.Pending
	if pending == nil || pending.StagedID != previous.StagedID {
		return "", contractError("STATE_CONFLICT", "system-installer handoff state was replaced", originalErr)
	}
	if pending.Status == "handoff" {
		return pending.HandoffReference, nil
	}
	next := latest.State
	next.Pending = clonePending(pending)
	next.Pending.Status = "handoff"
	next.Pending.HandoffAt = canonicalNow(service.clock().UTC())
	next.Pending.HandoffReference = receipt.Reference
	if err := validateProductState(next); err != nil {
		return "", err
	}
	if _, err := service.store.CompareAndSwap(ctx, latest.Revision, next); err != nil {
		return "", contractError("STATE_CONFLICT", "system-installer handoff recovery changed concurrently", errors.Join(originalErr, err))
	}
	return receipt.Reference, nil
}

// CommitInstalled accepts no caller-supplied path or version. It asks the
// injected product-integrity verifier to inspect the newly installed bundle.
// Durable status is not installation evidence, so an interrupted state
// transition may also be reconciled when the verifier proves the exact pending
// product is already installed.
func (service *Service) CommitInstalled(ctx context.Context) (CommitResult, error) {
	stored, err := service.store.Load(ctx)
	if err != nil {
		return CommitResult{}, contractError("STATE_LOAD", "cannot load authoritative update state", err)
	}
	if err := validateProductState(stored.State); err != nil {
		return CommitResult{}, err
	}
	return service.commitInstalled(ctx, stored)
}

func (service *Service) commitInstalled(ctx context.Context, stored StoredProductState) (CommitResult, error) {
	pending := stored.State.Pending
	if pending == nil {
		return CommitResult{}, contractError("INSTALL_PENDING", "no update is pending product verification", nil)
	}
	evidence, err := service.verifier.VerifyInstalledProduct(ctx, PendingProductEvidence{
		Product: pending.Product, Version: pending.Version, BuildNumber: pending.BuildNumber,
		StagedID: pending.StagedID, ArtifactSize: pending.Artifact.Size,
		ArtifactSHA256: pending.Artifact.SHA256,
	})
	if err != nil {
		return CommitResult{}, contractError("PRODUCT_INTEGRITY", "installed product verification failed", err)
	}
	if !evidence.IntegrityVerified || !digestPattern.MatchString(evidence.IntegrityDigest) {
		return CommitResult{}, contractError("PRODUCT_INTEGRITY", "installed product integrity was not proven", nil)
	}
	if evidence.IntegrityDigest != pending.Artifact.SHA256 {
		return CommitResult{}, contractError("PRODUCT_INTEGRITY", "installed product digest evidence does not match pending artifact", nil)
	}
	if evidence.Product != service.config.Product || evidence.Product != pending.Product {
		return CommitResult{}, contractError("PRODUCT_IDENTITY", "installed product does not match pending product", nil)
	}
	if evidence.Version != pending.Version || evidence.BuildNumber != pending.BuildNumber {
		return CommitResult{}, contractError("PRODUCT_VERSION", "installed version/build does not match pending update", nil)
	}
	next := stored.State
	next.Accepted = cloneContractState(next.Accepted)
	next.Accepted.Feeds[pending.FeedID] = FeedState{
		Sequence: pending.FeedSequence, Digest: pending.FeedDigest,
		ReleaseID: pending.ReleaseID, Version: pending.Version,
		BuildNumber: pending.BuildNumber, AcceptedAt: canonicalNow(service.clock().UTC()),
	}
	next.Pending = nil
	if err := validateProductState(next); err != nil {
		return CommitResult{}, err
	}
	if _, err := service.store.CompareAndSwap(ctx, stored.Revision, next); err != nil {
		return CommitResult{}, contractError("STATE_CONFLICT", "accepted state changed concurrently", err)
	}
	service.removeStage(pending.StagedID)
	return CommitResult{FeedID: pending.FeedID, Sequence: pending.FeedSequence, Version: pending.Version, BuildNumber: pending.BuildNumber, Accepted: true}, nil
}

// RecoverPendingInstall reconciles the durable install handoff during
// protected startup. It never accepts an update merely because Installer.app
// was opened: CommitInstalled still requires exact, fresh integrity evidence
// from the installed product. If the handoff was cancelled or failed, the
// pending update becomes retryable so a later explicit Install call can safely
// hand the already-verified package to the system installer again.
func (service *Service) RecoverPendingInstall(ctx context.Context) (InstallRecoveryResult, error) {
	stored, err := service.store.Load(ctx)
	if err != nil {
		return InstallRecoveryResult{}, contractError("STATE_LOAD", "cannot load authoritative update state", err)
	}
	if err := validateProductState(stored.State); err != nil {
		return InstallRecoveryResult{}, err
	}
	pending := stored.State.Pending
	if pending == nil {
		return InstallRecoveryResult{}, nil
	}

	committed, err := service.commitInstalled(ctx, stored)
	if err == nil {
		return InstallRecoveryResult{StagedID: pending.StagedID, Accepted: committed.Accepted}, nil
	}
	if ctx.Err() != nil {
		return InstallRecoveryResult{}, err
	}
	switch ErrorCode(err) {
	case "PRODUCT_INTEGRITY", "PRODUCT_IDENTITY", "PRODUCT_VERSION":
		// These errors prove only that the pending version is not currently
		// installed and trusted. They must never advance accepted state.
	default:
		if ErrorCode(err) == "STATE_CONFLICT" {
			latest, loadErr := service.store.Load(ctx)
			if loadErr == nil {
				if stateErr := validateProductState(latest.State); stateErr != nil {
					return InstallRecoveryResult{}, stateErr
				}
				if pendingWasAccepted(latest.State, pending) {
					service.removeStage(pending.StagedID)
					return InstallRecoveryResult{StagedID: pending.StagedID, Accepted: true}, nil
				}
			}
		}
		return InstallRecoveryResult{}, err
	}
	if pending.Status == "staged" {
		return InstallRecoveryResult{StagedID: pending.StagedID}, nil
	}
	if pending.Status == "installing" {
		next := stored.State
		next.Pending = clonePending(pending)
		next.Pending.Status = "staged"
		if err := validateProductState(next); err != nil {
			return InstallRecoveryResult{}, err
		}
		if _, casErr := service.store.CompareAndSwap(ctx, stored.Revision, next); casErr != nil {
			latest, loadErr := service.store.Load(ctx)
			if loadErr == nil {
				if stateErr := validateProductState(latest.State); stateErr != nil {
					return InstallRecoveryResult{}, stateErr
				}
				if pendingWasAccepted(latest.State, pending) {
					service.removeStage(pending.StagedID)
					return InstallRecoveryResult{StagedID: pending.StagedID, Accepted: true}, nil
				}
				if latest.State.Pending != nil && latest.State.Pending.StagedID == pending.StagedID && latest.State.Pending.Status == "staged" {
					return InstallRecoveryResult{StagedID: pending.StagedID, Retryable: true}, nil
				}
			}
			return InstallRecoveryResult{}, contractError("STATE_CONFLICT", "interrupted install recovery changed concurrently", casErr)
		}
		return InstallRecoveryResult{StagedID: pending.StagedID, Retryable: true}, nil
	}
	if pending.Status == "retryable" {
		return InstallRecoveryResult{StagedID: pending.StagedID, Retryable: true}, nil
	}

	next := stored.State
	next.Pending = clonePending(pending)
	next.Pending.Status = "retryable"
	if err := validateProductState(next); err != nil {
		return InstallRecoveryResult{}, err
	}
	if _, casErr := service.store.CompareAndSwap(ctx, stored.Revision, next); casErr != nil {
		latest, loadErr := service.store.Load(ctx)
		if loadErr == nil {
			if stateErr := validateProductState(latest.State); stateErr != nil {
				return InstallRecoveryResult{}, stateErr
			}
			if pendingWasAccepted(latest.State, pending) {
				service.removeStage(pending.StagedID)
				return InstallRecoveryResult{StagedID: pending.StagedID, Accepted: true}, nil
			}
			if latest.State.Pending != nil && latest.State.Pending.StagedID == pending.StagedID && latest.State.Pending.Status == "retryable" {
				return InstallRecoveryResult{StagedID: pending.StagedID, Retryable: true}, nil
			}
		}
		return InstallRecoveryResult{}, contractError("STATE_CONFLICT", "unverified install recovery changed concurrently", casErr)
	}
	return InstallRecoveryResult{StagedID: pending.StagedID, Retryable: true}, nil
}

func (service *Service) loadOrCreateState(ctx context.Context) (StoredProductState, error) {
	stored, err := service.store.Load(ctx)
	if err == nil {
		if err := validateProductState(stored.State); err != nil {
			return StoredProductState{}, err
		}
		return stored, nil
	}
	if !errors.Is(err, ErrStateNotFound) {
		return StoredProductState{}, contractError("STATE_LOAD", "cannot load authoritative update state", err)
	}
	accepted, err := createInitialState(service.bootstrap)
	if err != nil {
		return StoredProductState{}, err
	}
	initial := ProductState{CurrentKeyring: service.bootstrap, Accepted: accepted}
	stored, err = service.store.CompareAndSwap(ctx, "", initial)
	if err != nil {
		return StoredProductState{}, contractError("STATE_CONFLICT", "cannot initialize authoritative update state", err)
	}
	return stored, nil
}

func validateProductState(state ProductState) error {
	if err := validateKeyring(state.CurrentKeyring); err != nil {
		return err
	}
	if err := verifyPinnedKeyring(state.CurrentKeyring, state.Accepted); err != nil {
		return err
	}
	if state.Pending == nil {
		return nil
	}
	pending := state.Pending
	if !stagedIDPattern.MatchString(pending.StagedID) || (pending.Status != "staged" && pending.Status != "installing" && pending.Status != "handoff" && pending.Status != "retryable") {
		return contractError("STATE_PENDING", "pending update has invalid identity or status", nil)
	}
	if pending.Product == "" {
		return contractError("STATE_PENDING", "pending product identity is missing", nil)
	}
	if err := validateID(pending.FeedID, "STATE_PENDING", "pending feedId"); err != nil {
		return err
	}
	if err := requirePositiveSafe(pending.FeedSequence, "STATE_PENDING", "pending feed sequence"); err != nil {
		return err
	}
	if !digestPattern.MatchString(pending.FeedDigest) || pending.KeyringSequence != state.CurrentKeyring.Sequence {
		return contractError("STATE_PENDING", "pending digest or keyring sequence is invalid", nil)
	}
	if err := validateRelease(Release{ReleaseID: pending.ReleaseID, Version: pending.Version, BuildNumber: pending.BuildNumber, PublishedAt: pending.StagedAt, Artifacts: []Artifact{pending.Artifact}}); err != nil {
		return err
	}
	if _, err := parseTimestamp(pending.StagedAt, "pending.stagedAt"); err != nil {
		return err
	}
	if pending.Status == "handoff" || pending.Status == "retryable" {
		if pending.HandoffReference == "" {
			return contractError("STATE_PENDING", "handoff reference is required", nil)
		}
		if _, err := parseTimestamp(pending.HandoffAt, "pending.handoffAt"); err != nil {
			return err
		}
	} else if pending.HandoffAt != "" || pending.HandoffReference != "" {
		return contractError("STATE_PENDING", "non-handoff pending update contains handoff evidence", nil)
	}
	return nil
}

func clonePending(pending *PendingUpdate) *PendingUpdate {
	if pending == nil {
		return nil
	}
	copy := *pending
	if pending.Artifact.Signature != nil {
		signature := *pending.Artifact.Signature
		copy.Artifact.Signature = &signature
	}
	return &copy
}

func pendingWasAccepted(state ProductState, pending *PendingUpdate) bool {
	if pending == nil {
		return false
	}
	accepted, ok := state.Accepted.Feeds[pending.FeedID]
	return ok &&
		accepted.Sequence == pending.FeedSequence &&
		accepted.Digest == pending.FeedDigest &&
		accepted.ReleaseID == pending.ReleaseID &&
		accepted.Version == pending.Version &&
		accepted.BuildNumber == pending.BuildNumber
}

func (service *Service) resolveCandidate(ctx context.Context, stored StoredProductState) (candidate, error) {
	download, err := service.client.FetchFeed(ctx)
	if err != nil {
		return candidate{}, contractError("FEED_FETCH", "fixed feed fetch failed", err)
	}
	raw, err := service.readDocument(download)
	if err != nil {
		return candidate{}, err
	}
	feed, err := ParseFeed(raw)
	if err != nil {
		return candidate{}, err
	}
	if feed.Product != service.config.Product || feed.Channel != service.config.Channel || feed.FeedID != service.config.FeedID {
		return candidate{}, contractError("FEED_IDENTITY", "feed product/channel/feedId does not match build-time configuration", nil)
	}
	next := stored.State
	keyring := next.CurrentKeyring
	rotated := false
	if feed.KeyringSequence > keyring.Sequence {
		keyringDownload, err := service.client.FetchKeyring(ctx, feed.KeyringSequence)
		if err != nil {
			return candidate{}, contractError("KEYRING_FETCH", "fixed keyring fetch failed", err)
		}
		keyringRaw, err := service.readDocument(keyringDownload)
		if err != nil {
			return candidate{}, err
		}
		candidateKeyring, err := ParseKeyring(keyringRaw)
		if err != nil {
			return candidate{}, err
		}
		rotatedState, err := verifyKeyringRotation(keyring, candidateKeyring, next.Accepted, service.clock().UTC())
		if err != nil {
			return candidate{}, err
		}
		if candidateKeyring.Sequence != feed.KeyringSequence {
			return candidate{}, contractError("FEED_KEYRING_SEQUENCE", "candidate keyring sequence does not match feed", nil)
		}
		next.CurrentKeyring = candidateKeyring
		next.Accepted = rotatedState
		keyring = candidateKeyring
		rotated = true
	} else if feed.KeyringSequence != keyring.Sequence {
		return candidate{}, contractError("FEED_KEYRING_SEQUENCE", "feed references a different keyring sequence", nil)
	}
	feedDigest, err := service.verifyFeed(feed, keyring, next.Accepted)
	if err != nil {
		return candidate{}, err
	}
	artifact, err := service.selectArtifact(feed.Release.Artifacts)
	if err != nil {
		return candidate{}, err
	}
	issued, _ := parseTimestamp(feed.IssuedAt, "feed.issuedAt")
	if err := verifyArchiveSignature(feed, artifact, keyring, issued, service.clock().UTC()); err != nil {
		return candidate{}, err
	}
	if err := verifyPendingHistory(feed, feedDigest, next.Pending); err != nil {
		return candidate{}, err
	}
	return candidate{feed: feed, keyring: keyring, nextState: next, feedDigest: feedDigest, artifact: artifact, rotated: rotated}, nil
}

func (service *Service) verifyFeed(feed Feed, keyring Keyring, state ContractState) (string, error) {
	if err := verifyPinnedKeyring(keyring, state); err != nil {
		return "", err
	}
	now := service.clock().UTC()
	if _, err := verifyFreshDocument(keyring.IssuedAt, keyring.ExpiresAt, "keyring", now); err != nil {
		return "", err
	}
	issued, err := verifyFreshDocument(feed.IssuedAt, feed.ExpiresAt, "feed", now)
	if err != nil {
		return "", err
	}
	if feed.KeyringSequence != keyring.Sequence {
		return "", contractError("FEED_KEYRING_SEQUENCE", "feed references a different keyring sequence", nil)
	}
	if err := verifySignatureQuorum(feed, feed.Signatures, keyring, "feed", keyring.Threshold, issued, now); err != nil {
		return "", err
	}
	digest, err := DocumentDigest(feed)
	if err != nil {
		return "", err
	}
	if err := service.verifyFeedHistory(feed, digest, state); err != nil {
		return "", err
	}
	return digest, nil
}

func (service *Service) verifyFeedHistory(feed Feed, digest string, state ContractState) error {
	previous, found := state.Feeds[feed.FeedID]
	if !found {
		if feed.Sequence <= service.config.MinimumFeedSequence {
			return contractError("FEED_ROLLBACK", "feed sequence does not advance the build-time baseline", nil)
		}
		order, err := compareVersions(feed.Release.Version, service.config.CurrentVersion)
		if err != nil {
			return err
		}
		if order < 0 {
			return contractError("VERSION_ROLLBACK", "feed version rolls back the build-time baseline", nil)
		}
		if order == 0 && feed.Release.BuildNumber <= service.config.CurrentBuildNumber {
			return contractError("BUILD_REPLAY", "feed does not advance the build-time version/build", nil)
		}
		return nil
	}
	if feed.Sequence == previous.Sequence && digest == previous.Digest {
		return contractError("FEED_REPLAY", "feed was already accepted", nil)
	}
	if feed.Sequence <= previous.Sequence {
		return contractError("FEED_ROLLBACK", "feed sequence rolled back", nil)
	}
	order, err := compareVersions(feed.Release.Version, previous.Version)
	if err != nil {
		return err
	}
	if order < 0 {
		return contractError("VERSION_ROLLBACK", "feed version rolled back", nil)
	}
	if order == 0 && feed.Release.BuildNumber <= previous.BuildNumber {
		return contractError("BUILD_REPLAY", "feed does not advance version/build", nil)
	}
	return nil
}

func verifyPendingHistory(feed Feed, digest string, pending *PendingUpdate) error {
	if pending == nil || pending.FeedID != feed.FeedID || pending.FeedDigest == digest {
		return nil
	}
	if feed.Sequence <= pending.FeedSequence {
		return contractError("FEED_ROLLBACK", "feed does not advance the pending sequence", nil)
	}
	order, err := compareVersions(feed.Release.Version, pending.Version)
	if err != nil {
		return err
	}
	if order < 0 {
		return contractError("VERSION_ROLLBACK", "feed version rolls back the pending update", nil)
	}
	if order == 0 && feed.Release.BuildNumber <= pending.BuildNumber {
		return contractError("BUILD_REPLAY", "feed does not advance the pending version/build", nil)
	}
	return nil
}

func (service *Service) selectArtifact(artifacts []Artifact) (Artifact, error) {
	var universal *Artifact
	for index := range artifacts {
		artifact := artifacts[index]
		if artifact.Platform != service.config.Platform {
			continue
		}
		if artifact.Architecture == service.config.Architecture {
			if err := service.verifyMinimumOS(artifact); err != nil {
				return Artifact{}, err
			}
			return artifact, nil
		}
		if artifact.Architecture == "universal2" && service.config.Platform == "macos" {
			copy := artifact
			universal = &copy
		}
	}
	if universal != nil {
		if err := service.verifyMinimumOS(*universal); err != nil {
			return Artifact{}, err
		}
		return *universal, nil
	}
	return Artifact{}, contractError("ARTIFACT_TARGET", "feed has no artifact for the fixed platform/architecture", nil)
}

func (service *Service) verifyMinimumOS(artifact Artifact) error {
	order, err := compareOSVersions(service.config.CurrentOS, artifact.MinOS)
	if err != nil {
		return err
	}
	if order < 0 {
		return contractError("ARTIFACT_MIN_OS", "artifact requires a newer OS", nil)
	}
	return nil
}

func (service *Service) readDocument(download Download) ([]byte, error) {
	if err := service.validateDownload(download, service.config.MaxDocumentBytes); err != nil {
		if download.Body != nil {
			_ = download.Body.Close()
		}
		return nil, err
	}
	defer download.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(download.Body, service.config.MaxDocumentBytes+1))
	if err != nil {
		return nil, contractError("DOWNLOAD_PARTIAL", "document read was incomplete", err)
	}
	if int64(len(raw)) != download.ContentLength || int64(len(raw)) > service.config.MaxDocumentBytes {
		return nil, contractError("DOWNLOAD_PARTIAL", "document content length mismatch", nil)
	}
	return raw, nil
}

func (service *Service) validateDownload(download Download, maximum int64) error {
	if download.Body == nil || download.StatusCode != 200 {
		return contractError("DOWNLOAD_STATUS", "download did not return a complete 200 response", nil)
	}
	if download.Redirected {
		return contractError("DOWNLOAD_REDIRECT", "redirects are forbidden", nil)
	}
	if download.Origin != service.config.FeedOrigin || download.FinalOrigin != service.config.FeedOrigin {
		return contractError("DOWNLOAD_ORIGIN", "download origin differs from fixed build-time origin", nil)
	}
	if download.ContentLength <= 0 || download.ContentLength > maximum {
		return contractError("DOWNLOAD_SIZE", "download content length is missing or exceeds the fixed limit", nil)
	}
	return nil
}

func (service *Service) newStagedID() (string, error) {
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(service.random, bytes); err != nil {
		return "", contractError("STAGED_ID", "cannot generate opaque stagedId", err)
	}
	return hex.EncodeToString(bytes), nil
}

func (service *Service) secureStagingRoot() error {
	if err := os.MkdirAll(service.config.StagingRoot, 0o700); err != nil {
		return contractError("STAGING_DIRECTORY", "cannot create staging root", err)
	}
	info, err := os.Lstat(service.config.StagingRoot)
	if err != nil {
		return contractError("STAGING_DIRECTORY", "cannot inspect staging root", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return contractError("STAGING_SYMLINK", "staging root must be a real directory", nil)
	}
	if err := os.Chmod(service.config.StagingRoot, 0o700); err != nil {
		return contractError("STAGING_DIRECTORY", "cannot secure staging root", err)
	}
	return nil
}

func (service *Service) stageArchive(ctx context.Context, stagedID string, artifact Artifact) (returnErr error) {
	if artifact.Size > service.config.MaxArchiveBytes {
		return contractError("ARCHIVE_SIZE", "signed archive exceeds build-time limit", nil)
	}
	if err := service.secureStagingRoot(); err != nil {
		return err
	}
	directory := filepath.Join(service.config.StagingRoot, stagedID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return contractError("STAGING_DIRECTORY", "cannot create isolated staging directory", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			service.removeStage(stagedID)
		}
	}()
	download, err := service.client.FetchArchive(ctx, artifact.ArtifactID)
	if err != nil {
		return contractError("ARCHIVE_FETCH", "fixed archive fetch failed", err)
	}
	if err := service.validateDownload(download, service.config.MaxArchiveBytes); err != nil {
		if download.Body != nil {
			_ = download.Body.Close()
		}
		return err
	}
	if download.ContentLength != artifact.Size {
		_ = download.Body.Close()
		return contractError("ARCHIVE_SIZE", "archive content length differs from signed size", nil)
	}
	temporary := filepath.Join(directory, ".payload.tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = download.Body.Close()
		return contractError("STAGING_FILE", "cannot create atomic staging file", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(download.Body, artifact.Size+1))
	closeBodyErr := download.Body.Close()
	syncErr := file.Sync()
	closeFileErr := file.Close()
	if copyErr != nil || closeBodyErr != nil || syncErr != nil || closeFileErr != nil {
		return contractError("DOWNLOAD_PARTIAL", "archive stream did not complete durably", errors.Join(copyErr, closeBodyErr, syncErr, closeFileErr))
	}
	if written != artifact.Size {
		return contractError("ARCHIVE_SIZE", "archive size mismatch", nil)
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return contractError("ARCHIVE_DIGEST", "archive digest mismatch", nil)
	}
	destination := filepath.Join(directory, StagedArchiveFileName)
	if err := os.Rename(temporary, destination); err != nil {
		return contractError("STAGING_FILE", "cannot atomically publish staged archive", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return contractError("STAGING_DIRECTORY", "cannot open staging directory for sync", err)
	}
	syncDirectoryErr := directoryHandle.Sync()
	closeDirectoryErr := directoryHandle.Close()
	if syncDirectoryErr != nil || closeDirectoryErr != nil {
		return contractError("STAGING_DIRECTORY", "cannot durably sync staging directory", errors.Join(syncDirectoryErr, closeDirectoryErr))
	}
	cleanup = false
	return nil
}

func (service *Service) stagedArchivePath(pending *PendingUpdate) (string, error) {
	if pending == nil || !stagedIDPattern.MatchString(pending.StagedID) {
		return "", contractError("STAGED_ID", "pending stagedId is invalid", nil)
	}
	rootInfo, err := os.Lstat(service.config.StagingRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", contractError("STAGING_SYMLINK", "staging root is missing or not a real directory", err)
	}
	directory := filepath.Join(service.config.StagingRoot, pending.StagedID)
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return "", contractError("STAGING_SYMLINK", "staging directory is missing or not a real directory", err)
	}
	archivePath := filepath.Join(directory, StagedArchiveFileName)
	archiveInfo, err := os.Lstat(archivePath)
	if err != nil || !archiveInfo.Mode().IsRegular() || archiveInfo.Mode()&os.ModeSymlink != 0 {
		return "", contractError("ARCHIVE_FILE_TYPE", "staged archive must be a regular non-symlink file", err)
	}
	realArchive, err := filepath.EvalSymlinks(archivePath)
	realDirectory, directoryErr := filepath.EvalSymlinks(directory)
	if err != nil || directoryErr != nil || filepath.Dir(realArchive) != realDirectory {
		return "", contractError("ARCHIVE_PATH_ESCAPE", "staged archive escapes its isolated directory", errors.Join(err, directoryErr))
	}
	return archivePath, nil
}

func (service *Service) verifyStagedArchive(pending *PendingUpdate) error {
	archivePath, err := service.stagedArchivePath(pending)
	if err != nil {
		return err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return contractError("ARCHIVE_FILE", "cannot open staged archive", err)
	}
	hash := sha256.New()
	measured, copyErr := io.Copy(hash, io.LimitReader(file, pending.Artifact.Size+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return contractError("ARCHIVE_FILE", "cannot measure staged archive", errors.Join(copyErr, closeErr))
	}
	if measured != pending.Artifact.Size {
		return contractError("ARCHIVE_SIZE", "staged archive size mismatch", nil)
	}
	if hex.EncodeToString(hash.Sum(nil)) != pending.Artifact.SHA256 {
		return contractError("ARCHIVE_DIGEST", "staged archive digest mismatch", nil)
	}
	return nil
}

func (service *Service) removeStage(stagedID string) {
	if !stagedIDPattern.MatchString(stagedID) {
		return
	}
	directory := filepath.Join(service.config.StagingRoot, stagedID)
	_ = os.Remove(filepath.Join(directory, ".payload.tmp"))
	_ = os.Remove(filepath.Join(directory, StagedArchiveFileName))
	_ = os.Remove(directory)
}

func (pending PendingUpdate) String() string {
	return fmt.Sprintf("%s/%d %s+%d", pending.FeedID, pending.FeedSequence, pending.Version, pending.BuildNumber)
}
