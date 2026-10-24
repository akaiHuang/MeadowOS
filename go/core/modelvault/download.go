package modelvault

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func resolvedArtifactURL(entry verifiedEntry, artifact verifiedArtifact) (*url.URL, error) {
	resolvedPath := strings.NewReplacer(
		"{model_id}", entry.entry.ModelID,
		"{profile_id}", entry.entry.ProfileID,
		"{version}", entry.entry.Version,
	).Replace(artifact.entry.SourcePathTemplate)
	if !validSourcePathTemplate(resolvedPath) {
		return nil, ErrCatalogRejected
	}
	reference := &url.URL{Path: resolvedPath}
	resolved := entry.origin.ResolveReference(reference)
	if !sameOrigin(entry.origin, resolved) || resolved.RawQuery != "" || resolved.Fragment != "" {
		return nil, ErrCatalogRejected
	}
	return resolved, nil
}

func sameOrigin(expected, actual *url.URL) bool {
	if expected == nil || actual == nil || expected.Scheme != "https" || actual.Scheme != "https" {
		return false
	}
	normalizePort := func(value *url.URL) string {
		if value.Port() == "" {
			return "443"
		}
		return value.Port()
	}
	return strings.EqualFold(expected.Hostname(), actual.Hostname()) &&
		normalizePort(expected) == normalizePort(actual) && actual.User == nil
}

func fetchArtifact(
	ctx context.Context,
	transport http.RoundTripper,
	entry verifiedEntry,
	artifact verifiedArtifact,
	destination string,
) error {
	if transport == nil {
		return ErrUnavailable
	}
	current, err := resolvedArtifactURL(entry, artifact)
	if err != nil {
		return err
	}
	for redirect := 0; redirect <= maximumRedirects; redirect++ {
		if err := contextFailure(ctx); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return errors.Join(ErrDownloadRejected, err)
		}
		request.Header.Set("Accept-Encoding", "identity")
		response, err := transport.RoundTrip(request)
		if err != nil {
			if contextErr := contextFailure(ctx); contextErr != nil {
				return contextErr
			}
			return errors.Join(ErrDownloadRejected, err)
		}
		if response == nil || response.Body == nil {
			return ErrDownloadRejected
		}
		if response.StatusCode >= 300 && response.StatusCode <= 399 {
			location := response.Header.Get("Location")
			_ = response.Body.Close()
			if redirect == maximumRedirects || location == "" {
				return ErrDownloadRejected
			}
			next, err := current.Parse(location)
			if err != nil || !sameOrigin(entry.origin, next) || next.RawQuery != "" ||
				next.Fragment != "" || next.RawPath != "" ||
				!validSourcePathTemplate(next.EscapedPath()) {
				return ErrDownloadRejected
			}
			current = next
			continue
		}
		if response.StatusCode != http.StatusOK ||
			(response.ContentLength >= 0 && uint64(response.ContentLength) != artifact.entry.Bytes) ||
			(response.Header.Get("Content-Encoding") != "" &&
				response.Header.Get("Content-Encoding") != "identity") {
			_ = response.Body.Close()
			return ErrDownloadRejected
		}
		err = writeVerifiedReader(ctx, response.Body, artifact, destination)
		closeErr := response.Body.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return errors.Join(ErrDownloadRejected, closeErr)
		}
		return nil
	}
	return ErrDownloadRejected
}

func writeVerifiedReader(
	ctx context.Context,
	reader io.Reader,
	artifact verifiedArtifact,
	destination string,
) error {
	if reader == nil {
		return ErrInvalidArgument
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(ErrDownloadRejected, err)
	}
	valid := false
	defer func() {
		_ = file.Close()
		if !valid {
			if info, statErr := os.Lstat(destination); statErr == nil && info.Mode().IsRegular() {
				_ = wipeAndRemoveFile(destination, info.Size())
			}
		}
	}()
	totalHash := sha256.New()
	buffer := make([]byte, artifact.entry.ChunkBytes)
	defer wipeBytes(buffer)
	remaining := artifact.entry.Bytes
	for index, expected := range artifact.chunkDigests {
		if err := contextFailure(ctx); err != nil {
			return err
		}
		count := uint64(artifact.entry.ChunkBytes)
		if remaining < count {
			count = remaining
		}
		if count == 0 || count > uint64(len(buffer)) {
			return ErrDownloadRejected
		}
		chunk := buffer[:int(count)]
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return errors.Join(ErrDownloadRejected, err)
		}
		actual := sha256.Sum256(chunk)
		if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			return ErrDownloadRejected
		}
		if _, err := totalHash.Write(chunk); err != nil {
			return errors.Join(ErrDownloadRejected, err)
		}
		if _, err := file.Write(chunk); err != nil {
			return errors.Join(ErrDownloadRejected, err)
		}
		wipeBytes(chunk)
		remaining -= count
		_ = index
	}
	var extra [1]byte
	read, extraErr := reader.Read(extra[:])
	if read != 0 || !errors.Is(extraErr, io.EOF) || remaining != 0 {
		return ErrDownloadRejected
	}
	actualDigest := totalHash.Sum(nil)
	if subtle.ConstantTimeCompare(actualDigest, artifact.digest[:]) != 1 {
		return ErrDownloadRejected
	}
	if err := file.Sync(); err != nil {
		return errors.Join(ErrDownloadRejected, err)
	}
	if err := file.Close(); err != nil {
		return errors.Join(ErrDownloadRejected, err)
	}
	valid = true
	return nil
}

func stageArtifactPath(directory string, ordinal uint32) string {
	return filepath.Join(directory, fmt.Sprintf("%08d.plain", ordinal))
}

func contextFailure(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrCanceled, err)
	}
	return nil
}
