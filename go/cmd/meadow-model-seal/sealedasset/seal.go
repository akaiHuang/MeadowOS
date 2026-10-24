package sealedasset

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/hey-meadow/meadow-code/go/core/nativeassettransport"
)

// Seal writes a complete output directory through a same-filesystem partial
// directory and promotes it only after every asset and evidence file is synced.
func Seal(ctx context.Context, request SealRequest) (Evidence, error) {
	return seal(ctx, request)
}

func seal(ctx context.Context, request SealRequest) (Evidence, error) {
	if ctx == nil || request.SignatureVerifier == nil || request.Keys == nil ||
		request.ManifestPath == "" || request.ManifestSignaturePath == "" ||
		request.OutputDirectory == "" || len(request.AssetPaths) == 0 {
		return Evidence{}, ErrInvalidArgument
	}
	chunkBytes := request.ChunkBytes
	if chunkBytes == 0 {
		chunkBytes = DefaultChunkBytes
	}
	if chunkBytes == 0 || chunkBytes > nativeassettransport.MaxChunkBytes {
		return Evidence{}, ErrInvalidArgument
	}
	verified, err := LoadVerifiedManifestFiles(
		ctx, request.ManifestPath, request.ManifestSignaturePath, request.SignatureVerifier,
	)
	if err != nil {
		return Evidence{}, err
	}
	manifest := verified.SignedManifest()
	if len(request.AssetPaths) != len(manifest.Assets) {
		return Evidence{}, ErrAssetRejected
	}
	opened := make([]*os.File, len(manifest.Assets))
	defer func() {
		for _, file := range opened {
			if file != nil {
				_ = file.Close()
			}
		}
	}()
	for index, asset := range manifest.Assets {
		inputPath, exists := request.AssetPaths[asset.AssetID]
		if !exists || inputPath == "" {
			return Evidence{}, ErrAssetRejected
		}
		file, openErr := openAndVerifyAsset(ctx, inputPath, asset.PlaintextSize, asset.Binding.AssetDigest)
		if openErr != nil {
			return Evidence{}, ErrAssetRejected
		}
		opened[index] = file
	}

	master, err := request.Keys.MasterKey(ctx)
	if err != nil || len(master) != MasterKeyBytes {
		zero(master)
		return Evidence{}, ErrKeyUnavailable
	}
	defer zero(master)
	random := request.Random
	if random == nil {
		random = rand.Reader
	}
	parent := filepath.Dir(request.OutputDirectory)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Evidence{}, ErrOutputUnavailable
	}
	if _, err := os.Lstat(request.OutputDirectory); err == nil || !errors.Is(err, os.ErrNotExist) {
		return Evidence{}, ErrOutputUnavailable
	}
	partial, err := os.MkdirTemp(parent, ".meadow-model-seal-partial-")
	if err != nil {
		return Evidence{}, ErrOutputUnavailable
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(partial)
		}
	}()
	if err := os.Chmod(partial, 0o700); err != nil {
		return Evidence{}, ErrOutputUnavailable
	}
	evidence := Evidence{
		SchemaVersion:           EvidenceSchemaVersion,
		FormatVersion:           FormatVersion,
		ModelID:                 manifest.ModelID,
		ProfileRevision:         manifest.ProfileRevision,
		Backend:                 uint32(manifest.Backend),
		ProfileDigest:           hex.EncodeToString(manifest.ProfileDigest[:]),
		AssetSetDigest:          hex.EncodeToString(verified.assetSetDigest[:]),
		ManifestDigest:          hex.EncodeToString(verified.manifestDigest[:]),
		CanonicalManifestDigest: hex.EncodeToString(verified.canonicalDigest[:]),
		Assets:                  make([]AssetEvidence, 0, len(manifest.Assets)),
	}
	for index, asset := range manifest.Assets {
		if err := ctx.Err(); err != nil {
			return Evidence{}, errors.Join(ErrCanceled, err)
		}
		if _, err := opened[index].Seek(0, io.SeekStart); err != nil {
			return Evidence{}, ErrAssetRejected
		}
		assetEvidence, sealErr := sealAsset(
			ctx,
			filepath.Join(partial, SealedAssetFilename(asset.Ordinal)),
			opened[index],
			identityFor(verified, asset),
			chunkBytes,
			master,
			random,
		)
		if sealErr != nil {
			return Evidence{}, sealErr
		}
		evidence.Assets = append(evidence.Assets, assetEvidence)
	}
	evidence.EvidenceDigest, err = evidenceDigest(evidence)
	if err != nil {
		return Evidence{}, ErrOutputUnavailable
	}
	payload, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return Evidence{}, ErrOutputUnavailable
	}
	payload = append(payload, '\n')
	if err := writeExclusiveSync(filepath.Join(partial, EvidenceFilename), payload); err != nil {
		zero(payload)
		return Evidence{}, ErrOutputUnavailable
	}
	zero(payload)
	if err := os.Rename(partial, request.OutputDirectory); err != nil {
		return Evidence{}, ErrOutputUnavailable
	}
	committed = true
	return evidence, nil
}

func openAndVerifyAsset(ctx context.Context, inputPath string, size uint64, expected [sha256.Size]byte) (*os.File, error) {
	info, err := os.Lstat(inputPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || uint64(info.Size()) != size {
		return nil, ErrAssetRejected
	}
	file, err := os.Open(inputPath)
	if err != nil {
		return nil, ErrAssetRejected
	}
	fileInfo, err := file.Stat()
	if err != nil || !fileInfo.Mode().IsRegular() || uint64(fileInfo.Size()) != size {
		_ = file.Close()
		return nil, ErrAssetRejected
	}
	digest, err := hashReader(ctx, file)
	if err != nil || subtle.ConstantTimeCompare(digest[:], expected[:]) != 1 {
		_ = file.Close()
		return nil, ErrAssetRejected
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, ErrAssetRejected
	}
	return file, nil
}

func hashReader(ctx context.Context, reader io.Reader) ([sha256.Size]byte, error) {
	hasher := sha256.New()
	buffer := make([]byte, DefaultChunkBytes)
	defer zero(buffer)
	for {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			_, _ = hasher.Write(buffer[:count])
			zero(buffer[:count])
		}
		if err == io.EOF {
			break
		}
		if err != nil || count == 0 {
			return [sha256.Size]byte{}, ErrAssetRejected
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

func sealAsset(
	ctx context.Context,
	outputPath string,
	input *os.File,
	identity assetIdentity,
	chunkBytes uint32,
	master []byte,
	random io.Reader,
) (AssetEvidence, error) {
	var salt [32]byte
	var baseNonce [12]byte
	if _, err := io.ReadFull(random, salt[:]); err != nil {
		return AssetEvidence{}, ErrKeyUnavailable
	}
	if _, err := io.ReadFull(random, baseNonce[:]); err != nil {
		zero(salt[:])
		return AssetEvidence{}, ErrKeyUnavailable
	}
	headerKey := deriveKey(master, salt[:], "header", identity)
	chunkKey := deriveKey(master, salt[:], "chunk", identity)
	defer zero(headerKey)
	defer zero(chunkKey)
	headerAEAD, err := newGCM(headerKey)
	if err != nil {
		return AssetEvidence{}, ErrKeyUnavailable
	}
	chunkAEAD, err := newGCM(chunkKey)
	if err != nil {
		return AssetEvidence{}, ErrKeyUnavailable
	}
	metadata := metadataFor(identity, chunkBytes)
	metadataPlaintext, err := json.Marshal(metadata)
	if err != nil || len(metadataPlaintext) == 0 || len(metadataPlaintext) > maxEncryptedMetadata {
		zero(metadataPlaintext)
		return AssetEvidence{}, ErrAssetRejected
	}
	headerAAD := aad(identity, headerAADKind, 0, uint64(len(metadataPlaintext)))
	headerNonce := nonceFor(baseNonce, 0)
	metadataCiphertext := headerAEAD.Seal(nil, headerNonce[:], metadataPlaintext, headerAAD)
	zero(metadataPlaintext)
	zero(headerAAD)
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		zero(metadataCiphertext)
		return AssetEvidence{}, ErrOutputUnavailable
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	sealedHash := sha256.New()
	writer := io.MultiWriter(file, sealedHash)
	preamble := encodePreamble(salt, baseNonce, uint32(len(metadataCiphertext)))
	zero(salt[:])
	zero(baseNonce[:])
	if err := writeAll(writer, preamble[:]); err != nil || writeAll(writer, metadataCiphertext) != nil {
		zero(metadataCiphertext)
		return AssetEvidence{}, ErrOutputUnavailable
	}
	zero(metadataCiphertext)
	plaintextHash := sha256.New()
	buffer := make([]byte, chunkBytes)
	defer zero(buffer)
	count := chunkCount(identity.asset.PlaintextSize, chunkBytes)
	remaining := identity.asset.PlaintextSize
	for index := uint64(0); index < count; index++ {
		if err := ctx.Err(); err != nil {
			return AssetEvidence{}, errors.Join(ErrCanceled, err)
		}
		take := uint64(chunkBytes)
		if remaining < take {
			take = remaining
		}
		if _, err := io.ReadFull(input, buffer[:take]); err != nil {
			return AssetEvidence{}, ErrAssetRejected
		}
		_, _ = plaintextHash.Write(buffer[:take])
		chunkAAD := aad(identity, chunkAADKind, index, take)
		nonce := nonceFor(headerNonce, index+1)
		ciphertext := chunkAEAD.Seal(nil, nonce[:], buffer[:take], chunkAAD)
		zero(buffer[:take])
		zero(chunkAAD)
		var frameLength [4]byte
		binary.BigEndian.PutUint32(frameLength[:], uint32(len(ciphertext)))
		if err := writeAll(writer, frameLength[:]); err != nil || writeAll(writer, ciphertext) != nil {
			zero(ciphertext)
			return AssetEvidence{}, ErrOutputUnavailable
		}
		zero(ciphertext)
		remaining -= take
	}
	var secondDigest [sha256.Size]byte
	copy(secondDigest[:], plaintextHash.Sum(nil))
	if remaining != 0 || subtle.ConstantTimeCompare(secondDigest[:], identity.asset.Binding.AssetDigest[:]) != 1 {
		return AssetEvidence{}, ErrAssetRejected
	}
	if err := file.Sync(); err != nil || file.Close() != nil {
		closed = true
		return AssetEvidence{}, ErrOutputUnavailable
	}
	closed = true
	info, err := os.Lstat(outputPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return AssetEvidence{}, ErrOutputUnavailable
	}
	sealedDigest := sealedHash.Sum(nil)
	result := AssetEvidence{
		Ordinal:         identity.asset.Ordinal,
		AssetID:         identity.asset.AssetID,
		Role:            roleName(identity.asset.Role),
		Required:        identity.asset.Required,
		PlaintextBytes:  identity.asset.PlaintextSize,
		PlaintextSHA256: hex.EncodeToString(identity.asset.Binding.AssetDigest[:]),
		ChunkBytes:      chunkBytes,
		ChunkCount:      count,
		SealedBytes:     uint64(info.Size()),
		SealedSHA256:    hex.EncodeToString(sealedDigest),
	}
	zero(sealedDigest)
	return result, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrNoProgress
		}
		payload = payload[written:]
	}
	return nil
}

func writeExclusiveSync(outputPath string, payload []byte) error {
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := writeAll(file, payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
