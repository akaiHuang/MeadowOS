package modelconnection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxCatalogBytes = 1 << 20
const maxCredentialBytes = 16 << 10

func Load(path string) (*Catalog, error) {
	data, err := readPrivate(path, maxCatalogBytes)
	if err != nil {
		return nil, err
	}
	var catalog Catalog
	if !utf8.Valid(data) || rejectDuplicateFields(data) != nil {
		return nil, fmt.Errorf("invalid model connection catalog JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&catalog) != nil {
		return nil, fmt.Errorf("invalid model connection catalog fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("model connection catalog must contain one JSON object")
	}
	if err := Validate(&catalog); err != nil {
		return nil, err
	}
	catalog.CoalesceServices()
	return &catalog, nil
}

func Save(path string, catalog *Catalog) error {
	return SaveContext(context.Background(), path, catalog)
}

func SaveContext(ctx context.Context, path string, catalog *Catalog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := Validate(catalog); err != nil {
		return err
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil || len(data) >= maxCatalogBytes {
		return fmt.Errorf("model connection catalog exceeds storage limit")
	}
	return writePrivate(ctx, path, append(data, '\n'))
}

// Credential returns an empty value when this connection has no saved key.
// Callers decide whether their provider accepts unauthenticated requests.
func Credential(catalogPath, connectionID string) (string, error) {
	path, err := credentialPath(catalogPath, connectionID)
	if err != nil {
		return "", err
	}
	if err := checkCredentialDir(filepath.Dir(path), false); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	data, err := readPrivate(path, maxCredentialBytes)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	key := string(data)
	if !validCredential(key) {
		return "", fmt.Errorf("stored model credential is invalid")
	}
	return key, nil
}

// SaveCredential atomically replaces a key. An empty key removes the saved
// credential. Catalog paths are never used as credential-file references.
func SaveCredential(catalogPath, connectionID, key string) error {
	return SaveCredentialContext(context.Background(), catalogPath, connectionID, key)
}

func DeleteCredential(catalogPath, connectionID string) error {
	return SaveCredentialContext(context.Background(), catalogPath, connectionID, "")
}

func SaveCredentialContext(ctx context.Context, catalogPath, connectionID, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := credentialPath(catalogPath, connectionID)
	if err != nil {
		return err
	}
	if key != "" && !validCredential(key) {
		return fmt.Errorf("model credential is invalid")
	}
	if err := checkCredentialDir(filepath.Dir(path), key != ""); err != nil {
		if key == "" && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if key == "" {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("model credential is not a regular file")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("model credential could not be removed")
		}
		return nil
	}
	return writePrivate(ctx, path, []byte(key))
}

func credentialPath(catalogPath, id string) (string, error) {
	if !filepath.IsAbs(catalogPath) || !validID(id) {
		return "", fmt.Errorf("absolute catalog path and valid connection ID are required")
	}
	// Hashing avoids filesystem case-folding collisions between connection IDs.
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(catalogPath+".credentials", hex.EncodeToString(digest[:])+".key"), nil
}

func validCredential(key string) bool {
	return len(key) <= maxCredentialBytes && validText(key, maxCredentialBytes) && strings.TrimSpace(key) == key
}

func checkCredentialDir(path string, create bool) error {
	if create {
		if err := ensureParent(path); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("credential directory is unavailable")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return fmt.Errorf("credential directory is unavailable")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("credential directory must be a private directory")
	}
	return nil
}

func ensureParent(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("model connection path must be absolute")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return fmt.Errorf("model connection directory is unavailable")
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("model connection directory must not be a symlink or writable by others")
	}
	return nil
}

func readPrivate(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("model connection path must be absolute")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("model connection directory is unavailable")
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("model connection directory is unsafe")
	}
	before, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("model connection file is unavailable")
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > limit {
		return nil, fmt.Errorf("model connection file must be a bounded private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("model connection file is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || opened.Size() > limit {
		return nil, fmt.Errorf("model connection file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("model connection file exceeds read limit")
	}
	return data, nil
}

func writePrivate(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureParent(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("existing model connection file must be private and regular")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("model connection file is unavailable")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".model-connection-*")
	if err != nil {
		return fmt.Errorf("model connection temporary file is unavailable")
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return fmt.Errorf("model connection permissions could not be set")
	}
	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("model connection write failed")
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("model connection sync failed")
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("model connection close failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("model connection commit failed")
	}
	return nil
}

// encoding/json accepts duplicate keys. Reject them before typed decoding so a
// catalog has a single interpretation for selections and connection metadata.
func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] || name != strings.ToLower(name) {
					return fmt.Errorf("duplicate JSON field")
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
