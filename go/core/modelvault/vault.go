package modelvault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
)

type vaultState struct {
	SchemaVersion   uint32            `json:"schema_version"`
	CatalogSequence uint64            `json:"catalog_sequence"`
	CatalogDigest   string            `json:"catalog_digest"`
	Installed       []installedRecord `json:"installed"`
}

type installedRecord struct {
	ModelID        string                 `json:"model_id"`
	Version        string                 `json:"version"`
	ProfileID      string                 `json:"profile_id"`
	ProfileDigest  string                 `json:"profile_digest"`
	LicenseID      string                 `json:"license_id"`
	ManifestDigest string                 `json:"manifest_digest"`
	InstalledAt    int64                  `json:"installed_at_epoch_seconds"`
	Generation     string                 `json:"generation"`
	Assets         []installedAssetRecord `json:"assets"`
}

type installedAssetRecord struct {
	Ordinal uint32 `json:"ordinal"`
	AssetID string `json:"asset_id"`
	Role    uint32 `json:"role"`
	Bytes   uint64 `json:"bytes"`
	Digest  string `json:"digest"`
}

type Vault struct {
	config                Config
	root                  string
	vaultID               [sha256.Size]byte
	acceptedCatalogSeq    uint64
	acceptedCatalogDigest [sha256.Size]byte
	mutex                 sync.Mutex
}

var errLocalStateDamaged = errors.New("model vault local state is damaged")

func Open(config Config) (*Vault, error) {
	if !validateBundleID(config.BundleID) || config.Clock == nil ||
		config.SequenceGuard == nil || config.Sealer == nil ||
		config.LicenseVerifier == nil || config.RemovalAuthorizer == nil {
		return nil, ErrInvalidArgument
	}
	root, err := platformVaultRoot(config.BundleID)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := ensurePrivateDirectory(filepath.Join(root, stagingDirectory)); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := ensurePrivateDirectory(filepath.Join(root, modelsDirectory)); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	vault := &Vault{config: cloneConfig(config), root: root, vaultID: sha256.Sum256([]byte(root))}
	if err := vault.withLock(context.Background(), func() error {
		return vault.recoverLocked()
	}); err != nil {
		return nil, err
	}
	return vault, nil
}

func (vault *Vault) AcceptCatalog(
	ctx context.Context,
	payload []byte,
	signature CatalogSignature,
) (VerifiedCatalog, error) {
	if vault == nil || ctx == nil {
		return VerifiedCatalog{}, ErrInvalidArgument
	}
	now := vault.config.Clock.Now()
	catalog, entries, digest, err := verifyCatalogPayload(
		ctx, append([]byte(nil), payload...), cloneSignature(signature), vault.config.Keyring, now,
	)
	if err != nil {
		return VerifiedCatalog{}, err
	}
	err = vault.withLock(ctx, func() error {
		if vault.acceptedCatalogSeq != 0 {
			if catalog.Sequence < vault.acceptedCatalogSeq {
				return ErrCatalogRollback
			}
			if catalog.Sequence == vault.acceptedCatalogSeq {
				if digestEqual(digest, vault.acceptedCatalogDigest) {
					return ErrCatalogReplay
				}
				return ErrCatalogRejected
			}
		}

		current, err := vault.config.SequenceGuard.CurrentCatalog(ctx, CatalogSequenceQuery{
			BundleID: vault.config.BundleID,
		})
		if err != nil {
			if contextErr := contextFailure(ctx); contextErr != nil {
				return contextErr
			}
			return errors.Join(ErrUnavailable, err)
		}
		if !validCatalogSequenceState(current) {
			return ErrUnavailable
		}
		if catalog.Sequence < current.Sequence {
			return ErrCatalogRollback
		}
		if catalog.Sequence == current.Sequence && !digestEqual(digest, current.CatalogDigest) {
			return ErrCatalogRejected
		}
		if catalog.Sequence > current.Sequence {
			if err := vault.config.SequenceGuard.AdvanceCatalog(ctx, CatalogSequenceAdvance{
				BundleID: vault.config.BundleID, Sequence: catalog.Sequence, CatalogDigest: digest,
			}); err != nil {
				if contextErr := contextFailure(ctx); contextErr != nil {
					return contextErr
				}
				return errors.Join(ErrCatalogRollback, err)
			}
		}

		state, stateErr := vault.readStateLocked()
		damaged := errors.Is(stateErr, errLocalStateDamaged)
		if stateErr != nil && !damaged {
			return stateErr
		}
		if damaged {
			state = vaultState{SchemaVersion: StateSchemaVersion, Installed: []installedRecord{}}
		}
		digestHex := hex.EncodeToString(digest[:])
		if damaged || state.CatalogSequence != catalog.Sequence || state.CatalogDigest != digestHex {
			state.CatalogSequence = catalog.Sequence
			state.CatalogDigest = digestHex
			if err := vault.writeStateLocked(state); err != nil {
				return err
			}
			if damaged {
				if err := vault.recoverLocked(); err != nil {
					return err
				}
			}
		}
		vault.acceptedCatalogSeq = catalog.Sequence
		vault.acceptedCatalogDigest = digest
		return nil
	})
	if err != nil {
		return VerifiedCatalog{}, err
	}
	return VerifiedCatalog{
		sequence: catalog.Sequence, digest: digest, entries: entries, vaultID: vault.vaultID,
	}, nil
}

func (vault *Vault) InstallDownloaded(
	ctx context.Context,
	catalog VerifiedCatalog,
	modelID, version string,
	acceptance LicenseAcceptance,
) (InstalledModelHandle, error) {
	if vault == nil || ctx == nil || vault.config.Transport == nil {
		return InstalledModelHandle{}, ErrUnavailable
	}
	entry, err := vault.catalogEntry(catalog, modelID, version)
	if err != nil {
		return InstalledModelHandle{}, err
	}
	if err := vault.verifyLicense(ctx, entry, acceptance); err != nil {
		return InstalledModelHandle{}, err
	}
	return vault.install(ctx, entry, func(plaintextDirectory string) error {
		for _, artifact := range entry.artifacts {
			if err := fetchArtifact(
				ctx, vault.config.Transport, entry, artifact,
				stageArtifactPath(plaintextDirectory, artifact.entry.Ordinal),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (vault *Vault) InstallImported(
	ctx context.Context,
	catalog VerifiedCatalog,
	modelID, version string,
	acceptance LicenseAcceptance,
	source UserImportSource,
) (InstalledModelHandle, error) {
	if vault == nil || ctx == nil || source == nil {
		return InstalledModelHandle{}, ErrInvalidArgument
	}
	entry, err := vault.catalogEntry(catalog, modelID, version)
	if err != nil {
		return InstalledModelHandle{}, err
	}
	if err := vault.verifyLicense(ctx, entry, acceptance); err != nil {
		return InstalledModelHandle{}, err
	}
	metadata, err := source.Metadata(ctx)
	if err != nil || !importMetadataMatches(entry, metadata) {
		return InstalledModelHandle{}, ErrImportRejected
	}
	return vault.install(ctx, entry, func(plaintextDirectory string) error {
		for _, artifact := range entry.artifacts {
			reader, descriptor, err := source.OpenArtifact(ctx, artifact.entry.Ordinal)
			if err != nil || reader == nil || !descriptorMatches(artifact, descriptor) {
				if reader != nil {
					_ = reader.Close()
				}
				return ErrImportRejected
			}
			err = writeVerifiedReader(
				ctx, reader, artifact,
				stageArtifactPath(plaintextDirectory, artifact.entry.Ordinal),
			)
			closeErr := reader.Close()
			if err != nil || closeErr != nil {
				return ErrImportRejected
			}
		}
		return nil
	})
}

func (vault *Vault) install(
	ctx context.Context,
	entry verifiedEntry,
	stagePlaintext func(string) error,
) (InstalledModelHandle, error) {
	var installed InstalledModelHandle
	err := vault.withLock(ctx, func() error {
		state, err := vault.readStateLocked()
		if err != nil {
			return err
		}
		if findInstalledRecord(state, entry.entry.ModelID, entry.entry.Version) >= 0 {
			return ErrAlreadyInstalled
		}
		token, err := randomToken()
		if err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		stageRoot := filepath.Join(vault.root, stagingDirectory, "install-"+token)
		plaintextDirectory := filepath.Join(stageRoot, "plaintext")
		sealedDirectory := filepath.Join(stageRoot, "sealed")
		if err := os.Mkdir(stageRoot, 0o700); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		defer secureRemoveTree(stageRoot)
		if err := os.Mkdir(plaintextDirectory, 0o700); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		if err := stagePlaintext(plaintextDirectory); err != nil {
			return err
		}
		if err := contextFailure(ctx); err != nil {
			return err
		}
		source := &stagedArtifactSource{directory: plaintextDirectory, entry: entry}
		request := sealRequestForEntry(entry)
		result, err := vault.config.Sealer.Seal(ctx, request, source)
		if err != nil {
			closeSealResult(&result)
			if contextErr := contextFailure(ctx); contextErr != nil {
				return contextErr
			}
			return errors.Join(ErrSealRejected, err)
		}
		defer closeSealResult(&result)
		if err := verifyStagedArtifacts(ctx, source); err != nil {
			return errors.Join(ErrSealRejected, err)
		}
		if err := secureRemoveTree(plaintextDirectory); err != nil {
			return errors.Join(ErrSealRejected, err)
		}
		if err := os.Mkdir(sealedDirectory, 0o700); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		record, err := writeSealResult(ctx, sealedDirectory, entry, result, token, vault.config.Clock.Now())
		if err != nil {
			return err
		}
		modelDirectory := filepath.Join(vault.root, modelsDirectory, entry.entry.ModelID)
		if err := ensurePrivateDirectory(modelDirectory); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		finalDirectory := filepath.Join(modelDirectory, record.Generation)
		if _, err := os.Lstat(finalDirectory); !errors.Is(err, os.ErrNotExist) {
			return ErrAlreadyInstalled
		}
		if err := os.Rename(sealedDirectory, finalDirectory); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = secureRemoveTree(finalDirectory)
			}
		}()
		if err := syncDirectory(modelDirectory); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		if err := os.Chmod(finalDirectory, installedDirectoryMode); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		if err := syncDirectory(finalDirectory); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		state.Installed = append(state.Installed, record)
		sortInstalledRecords(state.Installed)
		if err := vault.writeStateLocked(state); err != nil {
			return err
		}
		committed = true
		installed, err = handleFromRecord(record)
		return err
	})
	if err != nil {
		return InstalledModelHandle{}, err
	}
	return installed, nil
}

func (vault *Vault) List(ctx context.Context) ([]InstalledModelHandle, error) {
	if vault == nil || ctx == nil {
		return nil, ErrInvalidArgument
	}
	var result []InstalledModelHandle
	err := vault.withLock(ctx, func() error {
		state, err := vault.readStateLocked()
		if err != nil {
			return err
		}
		result = make([]InstalledModelHandle, 0, len(state.Installed))
		for _, record := range state.Installed {
			handle, err := handleFromRecord(record)
			if err != nil {
				return err
			}
			result = append(result, handle)
		}
		return nil
	})
	return result, err
}

func (vault *Vault) Status(
	ctx context.Context,
	modelID, version string,
) (ModelStatus, InstalledModelHandle, error) {
	if !validIdentifier(modelID) || !validIdentifier(version) {
		return ModelStatusUnavailable, InstalledModelHandle{}, ErrInvalidArgument
	}
	models, err := vault.List(ctx)
	if err != nil {
		return ModelStatusUnavailable, InstalledModelHandle{}, err
	}
	for _, model := range models {
		if model.ModelID == modelID && model.Version == version {
			return ModelStatusInstalled, model, nil
		}
	}
	return ModelStatusUnavailable, InstalledModelHandle{}, nil
}

func (vault *Vault) OpenManifest(
	ctx context.Context,
	handle InstalledModelHandle,
) (io.ReadCloser, error) {
	return vault.openInstalledFile(ctx, handle, sealedManifestName)
}

func (vault *Vault) OpenSealedAsset(
	ctx context.Context,
	handle InstalledModelHandle,
	ordinal uint32,
) (io.ReadCloser, InstalledAsset, error) {
	var selected *InstalledAsset
	for index := range handle.Assets {
		if handle.Assets[index].Ordinal == ordinal {
			copyValue := handle.Assets[index]
			selected = &copyValue
			break
		}
	}
	if selected == nil {
		return nil, InstalledAsset{}, ErrInvalidArgument
	}
	reader, err := vault.openInstalledFile(ctx, handle, sealedAssetName(ordinal))
	if err != nil {
		return nil, InstalledAsset{}, err
	}
	return reader, *selected, nil
}

func (vault *Vault) Remove(
	ctx context.Context,
	handle InstalledModelHandle,
) error {
	if vault == nil || ctx == nil {
		return ErrInvalidArgument
	}
	request := RemovalRequest{
		ModelID: handle.ModelID, Version: handle.Version,
		ProfileDigest: handle.ProfileDigest, ManifestDigest: handle.ManifestDigest,
	}
	if err := vault.config.RemovalAuthorizer.AuthorizeRemoval(ctx, request); err != nil {
		if contextErr := contextFailure(ctx); contextErr != nil {
			return contextErr
		}
		return errors.Join(ErrRemovalRejected, err)
	}
	return vault.withLock(ctx, func() error {
		state, err := vault.readStateLocked()
		if err != nil {
			return err
		}
		position := findInstalledRecord(state, handle.ModelID, handle.Version)
		if position < 0 || !recordMatchesHandle(state.Installed[position], handle) {
			return ErrNotInstalled
		}
		record := state.Installed[position]
		finalDirectory := filepath.Join(vault.root, modelsDirectory, record.ModelID, record.Generation)
		token, err := randomToken()
		if err != nil {
			return err
		}
		tombstone := filepath.Join(vault.root, stagingDirectory, "remove-"+token)
		if err := os.Chmod(finalDirectory, 0o700); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		if err := os.Rename(finalDirectory, tombstone); err != nil {
			_ = os.Chmod(finalDirectory, installedDirectoryMode)
			return errors.Join(ErrUnavailable, err)
		}
		state.Installed = append(state.Installed[:position], state.Installed[position+1:]...)
		if err := vault.writeStateLocked(state); err != nil {
			_ = os.Rename(tombstone, finalDirectory)
			return err
		}
		return secureRemoveTree(tombstone)
	})
}

func (vault *Vault) catalogEntry(
	catalog VerifiedCatalog,
	modelID, version string,
) (verifiedEntry, error) {
	if catalog.vaultID != vault.vaultID || catalog.sequence == 0 ||
		!validIdentifier(modelID) || !validIdentifier(version) {
		return verifiedEntry{}, ErrCatalogRejected
	}
	entry, ok := catalog.find(modelID, version)
	if !ok {
		return verifiedEntry{}, ErrCatalogRejected
	}
	return entry, nil
}

func (vault *Vault) verifyLicense(
	ctx context.Context,
	entry verifiedEntry,
	acceptance LicenseAcceptance,
) error {
	if acceptance.LicenseID != entry.entry.LicenseID ||
		subtle.ConstantTimeCompare(acceptance.LicenseDigest[:], entry.licenseDigest[:]) != 1 ||
		acceptance.AcceptedAt.IsZero() || acceptance.AcceptedAt.After(vault.config.Clock.Now()) ||
		!validSubject(acceptance.Subject) {
		return ErrLicenseRejected
	}
	binding := LicenseBinding{
		ModelID: entry.entry.ModelID, Version: entry.entry.Version,
		ProfileID: entry.entry.ProfileID, LicenseID: entry.entry.LicenseID,
		LicenseDigest: entry.licenseDigest,
	}
	if err := vault.config.LicenseVerifier.VerifyLicenseAcceptance(ctx, binding, acceptance); err != nil {
		if contextErr := contextFailure(ctx); contextErr != nil {
			return contextErr
		}
		return errors.Join(ErrLicenseRejected, err)
	}
	return contextFailure(ctx)
}

func (vault *Vault) openInstalledFile(
	ctx context.Context,
	handle InstalledModelHandle,
	filename string,
) (io.ReadCloser, error) {
	if vault == nil || ctx == nil || !validIdentifier(filename) {
		return nil, ErrInvalidArgument
	}
	var reader *os.File
	err := vault.withLock(ctx, func() error {
		state, err := vault.readStateLocked()
		if err != nil {
			return err
		}
		position := findInstalledRecord(state, handle.ModelID, handle.Version)
		if position < 0 || !recordMatchesHandle(state.Installed[position], handle) {
			return ErrNotInstalled
		}
		path := filepath.Join(
			vault.root, modelsDirectory, handle.ModelID, state.Installed[position].Generation, filename,
		)
		reader, err = directRegularFile(path)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (vault *Vault) withLock(ctx context.Context, operation func() error) error {
	if vault == nil || ctx == nil || operation == nil {
		return ErrInvalidArgument
	}
	if err := contextFailure(ctx); err != nil {
		return err
	}
	vault.mutex.Lock()
	defer vault.mutex.Unlock()
	lockPath := filepath.Join(vault.root, lockFilename)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	defer file.Close()
	if err := platformLock(file); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	defer platformUnlock(file)
	if err := contextFailure(ctx); err != nil {
		return err
	}
	return operation()
}

func (vault *Vault) readStateLocked() (vaultState, error) {
	path := filepath.Join(vault.root, stateFilename)
	file, err := directRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return vaultState{SchemaVersion: StateSchemaVersion, Installed: []installedRecord{}}, nil
	}
	if err != nil {
		return vaultState{}, errors.Join(ErrUnavailable, err)
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, maxCatalogBytes+1))
	if err != nil || len(payload) > maxCatalogBytes {
		wipeBytes(payload)
		return vaultState{}, ErrUnavailable
	}
	defer wipeBytes(payload)
	var state vaultState
	if err := decodeCanonicalJSON(payload, &state); err != nil ||
		state.SchemaVersion != StateSchemaVersion || len(state.Installed) > maxEntries {
		return vaultState{}, errors.Join(ErrUnavailable, errLocalStateDamaged)
	}
	if state.CatalogSequence == 0 {
		if state.CatalogDigest != "" {
			return vaultState{}, errors.Join(ErrUnavailable, errLocalStateDamaged)
		}
	} else if _, ok := parseDigest(state.CatalogDigest); !ok {
		return vaultState{}, errors.Join(ErrUnavailable, errLocalStateDamaged)
	}
	for _, record := range state.Installed {
		if _, err := handleFromRecord(record); err != nil {
			return vaultState{}, errors.Join(ErrUnavailable, errLocalStateDamaged)
		}
	}
	return state, nil
}

func (vault *Vault) writeStateLocked(state vaultState) error {
	state.SchemaVersion = StateSchemaVersion
	if state.Installed == nil {
		state.Installed = []installedRecord{}
	}
	return atomicWriteJSON(filepath.Join(vault.root, stateFilename), state, 0o600)
}

func validCatalogSequenceState(state CatalogSequenceState) bool {
	zero := [sha256.Size]byte{}
	if state.Sequence == 0 {
		return digestEqual(state.CatalogDigest, zero)
	}
	return !digestEqual(state.CatalogDigest, zero)
}

func digestEqual(left, right [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func (vault *Vault) recoverLocked() error {
	staging := filepath.Join(vault.root, stagingDirectory)
	entries, err := os.ReadDir(staging)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	for _, entry := range entries {
		if err := secureRemoveTree(filepath.Join(staging, entry.Name())); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
	}
	state, err := vault.readStateLocked()
	if err != nil {
		if errors.Is(err, errLocalStateDamaged) {
			return nil
		}
		return err
	}
	changed := false
	kept := state.Installed[:0]
	for _, record := range state.Installed {
		path := filepath.Join(vault.root, modelsDirectory, record.ModelID, record.Generation)
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			changed = true
			continue
		}
		kept = append(kept, record)
	}
	state.Installed = kept
	referenced := make(map[string]struct{}, len(state.Installed))
	for _, record := range state.Installed {
		referenced[filepath.Join(record.ModelID, record.Generation)] = struct{}{}
	}
	modelRoots, err := os.ReadDir(filepath.Join(vault.root, modelsDirectory))
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	for _, modelRoot := range modelRoots {
		modelPath := filepath.Join(vault.root, modelsDirectory, modelRoot.Name())
		if modelRoot.Type()&os.ModeSymlink != 0 || !modelRoot.IsDir() || !validIdentifier(modelRoot.Name()) {
			if err := secureRemoveTree(modelPath); err != nil {
				return errors.Join(ErrUnavailable, err)
			}
			continue
		}
		generations, err := os.ReadDir(modelPath)
		if err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		for _, generation := range generations {
			key := filepath.Join(modelRoot.Name(), generation.Name())
			if _, ok := referenced[key]; ok && generation.IsDir() && generation.Type()&os.ModeSymlink == 0 {
				continue
			}
			if err := secureRemoveTree(filepath.Join(modelPath, generation.Name())); err != nil {
				return errors.Join(ErrUnavailable, err)
			}
		}
	}
	if changed {
		return vault.writeStateLocked(state)
	}
	return nil
}

type stagedArtifactSource struct {
	directory string
	entry     verifiedEntry
}

func (source *stagedArtifactSource) OpenArtifact(
	ctx context.Context,
	ordinal uint32,
) (io.ReadCloser, ArtifactDescriptor, error) {
	if source == nil || ctx == nil || int(ordinal) >= len(source.entry.artifacts) {
		return nil, ArtifactDescriptor{}, ErrInvalidArgument
	}
	artifact := source.entry.artifacts[ordinal]
	if artifact.entry.Ordinal != ordinal {
		return nil, ArtifactDescriptor{}, ErrSealRejected
	}
	file, err := directRegularFile(stageArtifactPath(source.directory, ordinal))
	if err != nil {
		return nil, ArtifactDescriptor{}, ErrSealRejected
	}
	return file, descriptorForArtifact(artifact), nil
}

func sealRequestForEntry(entry verifiedEntry) SealRequest {
	request := SealRequest{
		ContractVersion: SealerContractV1, ModelID: entry.entry.ModelID,
		Version: entry.entry.Version, ProfileID: entry.entry.ProfileID,
		ProfileDigest: entry.profileDigest, LicenseID: entry.entry.LicenseID,
		LicenseDigest: entry.licenseDigest,
		Artifacts:     make([]ArtifactDescriptor, len(entry.artifacts)),
	}
	for index, artifact := range entry.artifacts {
		request.Artifacts[index] = descriptorForArtifact(artifact)
	}
	return request
}

func descriptorForArtifact(artifact verifiedArtifact) ArtifactDescriptor {
	return ArtifactDescriptor{
		Ordinal: artifact.entry.Ordinal, AssetID: artifact.entry.AssetID,
		Role: artifact.entry.Role, Bytes: artifact.entry.Bytes, Digest: artifact.digest,
	}
}

func descriptorMatches(artifact verifiedArtifact, descriptor ArtifactDescriptor) bool {
	return descriptor.Ordinal == artifact.entry.Ordinal &&
		descriptor.AssetID == artifact.entry.AssetID && descriptor.Role == artifact.entry.Role &&
		descriptor.Bytes == artifact.entry.Bytes &&
		subtle.ConstantTimeCompare(descriptor.Digest[:], artifact.digest[:]) == 1
}

func verifyStagedArtifacts(ctx context.Context, source *stagedArtifactSource) error {
	for _, artifact := range source.entry.artifacts {
		file, err := directRegularFile(stageArtifactPath(source.directory, artifact.entry.Ordinal))
		if err != nil {
			return err
		}
		err = verifyReader(ctx, file, artifact.entry.Bytes, artifact.digest)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func verifyReader(ctx context.Context, reader io.Reader, size uint64, expected [sha256.Size]byte) error {
	hash := sha256.New()
	written, err := io.CopyN(hash, &contextReader{ctx: ctx, reader: reader}, int64(size))
	if err != nil || uint64(written) != size {
		return ErrSealRejected
	}
	var extra [1]byte
	count, extraErr := reader.Read(extra[:])
	if count != 0 || !errors.Is(extraErr, io.EOF) ||
		subtle.ConstantTimeCompare(hash.Sum(nil), expected[:]) != 1 {
		return ErrSealRejected
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := contextFailure(reader.ctx); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func writeSealResult(
	ctx context.Context,
	directory string,
	entry verifiedEntry,
	result SealResult,
	token string,
	installedAt time.Time,
) (installedRecord, error) {
	if installedAt.IsZero() || len(result.SignedManifest) == 0 ||
		len(result.SignedManifest) > maxManifestBytes || len(result.Assets) == 0 ||
		len(result.Assets) > maxArtifacts {
		return installedRecord{}, ErrSealRejected
	}
	manifestDigest := sha256.Sum256(result.SignedManifest)
	if subtle.ConstantTimeCompare(manifestDigest[:], result.ManifestDigest[:]) != 1 ||
		subtle.ConstantTimeCompare(manifestDigest[:], entry.manifestDigest[:]) != 1 {
		return installedRecord{}, ErrSealRejected
	}
	manifestPath := filepath.Join(directory, sealedManifestName)
	manifestFile, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return installedRecord{}, errors.Join(ErrSealRejected, err)
	}
	if _, err := manifestFile.Write(result.SignedManifest); err != nil {
		_ = manifestFile.Close()
		return installedRecord{}, errors.Join(ErrSealRejected, err)
	}
	if err := manifestFile.Sync(); err != nil {
		_ = manifestFile.Close()
		return installedRecord{}, errors.Join(ErrSealRejected, err)
	}
	if err := manifestFile.Close(); err != nil {
		return installedRecord{}, errors.Join(ErrSealRejected, err)
	}
	if err := os.Chmod(manifestPath, 0o400); err != nil {
		return installedRecord{}, err
	}
	record := installedRecord{
		ModelID: entry.entry.ModelID, Version: entry.entry.Version,
		ProfileID: entry.entry.ProfileID, ProfileDigest: hex.EncodeToString(entry.profileDigest[:]),
		LicenseID: entry.entry.LicenseID, ManifestDigest: hex.EncodeToString(entry.manifestDigest[:]),
		InstalledAt: installedAt.Unix(), Generation: safeInstalledName(entry.entry.Version, entry.manifestDigest) + "-" + token[:8],
		Assets: make([]installedAssetRecord, 0, len(result.Assets)),
	}
	seenIDs := make(map[string]struct{}, len(result.Assets))
	for index := range result.Assets {
		asset := &result.Assets[index]
		if asset.Ordinal != uint32(index) || !validIdentifier(asset.AssetID) ||
			asset.Role < nativeassettransport.AssetRoleTokenizer ||
			asset.Role > nativeassettransport.AssetRoleWeight || asset.Bytes == 0 ||
			asset.Bytes > maxArtifactBytes || asset.Content == nil {
			return installedRecord{}, ErrSealRejected
		}
		if _, exists := seenIDs[asset.AssetID]; exists {
			return installedRecord{}, ErrSealRejected
		}
		seenIDs[asset.AssetID] = struct{}{}
		path := filepath.Join(directory, sealedAssetName(asset.Ordinal))
		if err := writeSealedOutput(ctx, path, asset); err != nil {
			return installedRecord{}, err
		}
		record.Assets = append(record.Assets, installedAssetRecord{
			Ordinal: asset.Ordinal, AssetID: asset.AssetID, Role: uint32(asset.Role),
			Bytes: asset.Bytes, Digest: hex.EncodeToString(asset.Digest[:]),
		})
	}
	if err := syncDirectory(directory); err != nil {
		return installedRecord{}, err
	}
	return record, nil
}

func writeSealedOutput(ctx context.Context, path string, asset *SealedAssetOutput) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(ErrSealRejected, err)
	}
	hash := sha256.New()
	written, copyErr := io.CopyN(io.MultiWriter(file, hash), &contextReader{ctx: ctx, reader: asset.Content}, int64(asset.Bytes))
	var extra [1]byte
	extraCount, extraErr := asset.Content.Read(extra[:])
	closeContentErr := asset.Content.Close()
	asset.Content = nil
	if copyErr != nil || uint64(written) != asset.Bytes || extraCount != 0 ||
		!errors.Is(extraErr, io.EOF) || closeContentErr != nil ||
		subtle.ConstantTimeCompare(hash.Sum(nil), asset.Digest[:]) != 1 {
		_ = file.Close()
		return ErrSealRejected
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o400)
}

func closeSealResult(result *SealResult) {
	if result == nil {
		return
	}
	for index := range result.Assets {
		if result.Assets[index].Content != nil {
			_ = result.Assets[index].Content.Close()
			result.Assets[index].Content = nil
		}
	}
}

func sealedAssetName(ordinal uint32) string {
	return fmt.Sprintf("asset-%08d.sealed", ordinal)
}

func importMetadataMatches(entry verifiedEntry, metadata ImportMetadata) bool {
	if metadata.ModelID != entry.entry.ModelID || metadata.Version != entry.entry.Version ||
		metadata.ProfileID != entry.entry.ProfileID || metadata.LicenseID != entry.entry.LicenseID ||
		subtle.ConstantTimeCompare(metadata.ProfileDigest[:], entry.profileDigest[:]) != 1 ||
		subtle.ConstantTimeCompare(metadata.LicenseDigest[:], entry.licenseDigest[:]) != 1 ||
		len(metadata.Artifacts) != len(entry.artifacts) {
		return false
	}
	for index, descriptor := range metadata.Artifacts {
		if !descriptorMatches(entry.artifacts[index], descriptor) {
			return false
		}
	}
	return true
}

func findInstalledRecord(state vaultState, modelID, version string) int {
	for index, record := range state.Installed {
		if record.ModelID == modelID && record.Version == version {
			return index
		}
	}
	return -1
}

func sortInstalledRecords(records []installedRecord) {
	sort.Slice(records, func(left, right int) bool {
		if records[left].ModelID == records[right].ModelID {
			return records[left].Version < records[right].Version
		}
		return records[left].ModelID < records[right].ModelID
	})
}

func handleFromRecord(record installedRecord) (InstalledModelHandle, error) {
	profile, ok := parseDigest(record.ProfileDigest)
	if !ok {
		return InstalledModelHandle{}, ErrUnavailable
	}
	manifest, ok := parseDigest(record.ManifestDigest)
	if !ok || !validIdentifier(record.ModelID) || !validIdentifier(record.Version) ||
		!validIdentifier(record.ProfileID) || !validIdentifier(record.LicenseID) ||
		!validIdentifier(record.Generation) || record.InstalledAt <= 0 ||
		len(record.Assets) == 0 || len(record.Assets) > maxArtifacts {
		return InstalledModelHandle{}, ErrUnavailable
	}
	handle := InstalledModelHandle{
		ModelID: record.ModelID, Version: record.Version, ProfileID: record.ProfileID,
		ProfileDigest: profile, LicenseID: record.LicenseID, ManifestDigest: manifest,
		InstalledAt: time.Unix(record.InstalledAt, 0), generation: record.Generation,
		Assets: make([]InstalledAsset, len(record.Assets)),
	}
	for index, asset := range record.Assets {
		digest, ok := parseDigest(asset.Digest)
		if !ok || asset.Ordinal != uint32(index) || !validIdentifier(asset.AssetID) ||
			asset.Role < 1 || asset.Role > 4 || asset.Bytes == 0 {
			return InstalledModelHandle{}, ErrUnavailable
		}
		handle.Assets[index] = InstalledAsset{
			Ordinal: asset.Ordinal, AssetID: asset.AssetID,
			Role: nativeassettransport.AssetRole(asset.Role), Bytes: asset.Bytes, Digest: digest,
		}
	}
	return handle, nil
}

func recordMatchesHandle(record installedRecord, handle InstalledModelHandle) bool {
	restored, err := handleFromRecord(record)
	return err == nil && restored.ModelID == handle.ModelID && restored.Version == handle.Version &&
		restored.generation == handle.generation &&
		subtle.ConstantTimeCompare(restored.ProfileDigest[:], handle.ProfileDigest[:]) == 1 &&
		subtle.ConstantTimeCompare(restored.ManifestDigest[:], handle.ManifestDigest[:]) == 1
}

func validSubject(subject string) bool {
	if len(subject) == 0 || len(subject) > maxLicenseTextBytes || !utf8.ValidString(subject) {
		return false
	}
	for _, value := range subject {
		if value < 0x20 || value == 0x7f {
			return false
		}
	}
	return strings.TrimSpace(subject) == subject
}

func cloneConfig(config Config) Config {
	copyValue := config
	copyValue.Keyring.Keys = make([]VerificationKey, len(config.Keyring.Keys))
	for index, key := range config.Keyring.Keys {
		copyValue.Keyring.Keys[index] = key
		copyValue.Keyring.Keys[index].PublicKey = append([]byte(nil), key.PublicKey...)
	}
	return copyValue
}

func cloneSignature(signature CatalogSignature) CatalogSignature {
	copyValue := signature
	copyValue.Signature = append([]byte(nil), signature.Signature...)
	return copyValue
}

func bytesReaderCloser(bytesValue []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(bytesValue))
}
