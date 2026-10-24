package session

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const (
	// Keep direct screenshots and persisted PDF pages at the serve boundary's
	// encoded-byte limit so a modified transcript cannot make LoadHistory read
	// an unbounded file. Decoded memory has a separate, stricter budget below.
	maxHistoryImageBytes       int64 = 1 << 20
	maxDirectHistoryImageBytes int64 = 1 << 20
	maxHistoryImageCount             = 16
	maxHistoryImageCandidates        = 64
	maxHistoryImagesBytes      int64 = 16 << 20
	// image.Decode can expand a very small, highly-compressed PNG into a much
	// larger in-memory image. Budget against the decoder's worst common native
	// representation (RGBA64/NRGBA64: eight bytes per pixel), not file size.
	maxHistoryImageBytesPerPixel int64 = 8
	maxHistoryImagesDecodedBytes int64 = 256 << 20
	maxHistoryImageDimension           = 32768
	maxHistoryImagePixels        int64 = 25_000_000
)

var historyImageNamePattern = regexp.MustCompile(
	`^(?:attachment-(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9]+)-[1-9][0-9]*|pdf-page-[0-9]{3}-(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9]+))\.(?:jpg|jpeg|png|webp|gif)$`,
)

type historyImageReader struct {
	root  *os.Root
	cache map[string]historyImage
}

type historyImage struct {
	part         provider.MessageContentPart
	bytes        int64
	decodedBytes int64
	ok           bool
}

// rehydrateHistoryImages turns durable attachment basenames back into the
// multipart provider content used by the original turn. Invalid, missing or
// unsafe references fail closed as text-only history; one damaged attachment
// must not make an otherwise usable legacy conversation impossible to open.
func rehydrateHistoryImages(chatPath string, messages []provider.Message, refs map[int][]string) []provider.Message {
	if len(messages) == 0 || len(refs) == 0 {
		return messages
	}
	reader := openHistoryImageReader(chatPath)
	if reader == nil {
		return removeEmptyHistoryUsers(messages)
	}
	defer reader.root.Close()

	remainingCount := maxHistoryImageCount
	remainingBytes := maxHistoryImagesBytes
	remainingDecodedBytes := maxHistoryImagesDecodedBytes
	attempted := 0
	// Prefer the newest retained history when a tampered or exceptionally long
	// transcript exceeds the bounded hydration budget.
	for messageIndex := len(messages) - 1; messageIndex >= 0; messageIndex-- {
		names := refs[messageIndex]
		if len(names) == 0 || remainingCount == 0 || remainingBytes == 0 || remainingDecodedBytes == 0 ||
			attempted == maxHistoryImageCandidates {
			continue
		}
		capacity := min(len(names), remainingCount)
		images := make([]provider.MessageContentPart, 0, capacity)
		for _, name := range names {
			if remainingCount == 0 || remainingDecodedBytes == 0 || attempted == maxHistoryImageCandidates {
				break
			}
			attempted++
			image := reader.read(name, remainingBytes, remainingDecodedBytes)
			if !image.ok {
				continue
			}
			images = append(images, image.part)
			remainingCount--
			remainingBytes -= image.bytes
			remainingDecodedBytes -= image.decodedBytes
		}
		if len(images) == 0 {
			continue
		}
		parts := make([]provider.MessageContentPart, 0, len(images)+1)
		if messages[messageIndex].Content != "" {
			parts = append(parts, provider.MessageContentPart{
				Type: "text", Text: messages[messageIndex].Content,
			})
		}
		parts = append(parts, images...)
		messages[messageIndex].ContentParts = parts
	}
	return removeEmptyHistoryUsers(messages)
}

func removeEmptyHistoryUsers(messages []provider.Message) []provider.Message {
	filtered := messages[:0]
	for _, message := range messages {
		// The legacy loader already omitted empty user text records. Preserve that
		// behavior when an image-only record has no safe attachment to hydrate.
		if message.Role == "user" && message.Content == "" && len(message.ContentParts) == 0 {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func openHistoryImageReader(chatPath string) *historyImageReader {
	cleanPath := filepath.Clean(strings.TrimSpace(chatPath))
	chatName := filepath.Base(cleanPath)
	if filepath.Base(filepath.Dir(cleanPath)) != "chats" ||
		!strings.HasSuffix(chatName, ".jsonl") {
		return nil
	}
	sessionID := strings.TrimSuffix(chatName, ".jsonl")
	if sessionID == "" || sessionID == "." || sessionID == ".." ||
		filepath.Base(sessionID) != sessionID || strings.ContainsAny(sessionID, `/\\`) ||
		strings.IndexByte(sessionID, 0) >= 0 {
		return nil
	}

	projectRoot := openVerifiedHistoryRoot(filepath.Dir(filepath.Dir(cleanPath)))
	if projectRoot == nil {
		return nil
	}
	defer projectRoot.Close()
	sessionsRoot := openVerifiedHistoryChildRoot(projectRoot, "sessions")
	if sessionsRoot == nil {
		return nil
	}
	defer sessionsRoot.Close()
	sessionRoot := openVerifiedHistoryChildRoot(sessionsRoot, sessionID)
	if sessionRoot == nil {
		return nil
	}
	defer sessionRoot.Close()
	artifactRoot := openVerifiedHistoryChildRoot(sessionRoot, "artifacts")
	if artifactRoot == nil {
		return nil
	}
	return &historyImageReader{root: artifactRoot, cache: map[string]historyImage{}}
}

func openVerifiedHistoryRoot(path string) *os.Root {
	before, err := os.Lstat(path)
	if !safeHistoryDirectory(before, err) {
		return nil
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil
	}
	return root
}

func openVerifiedHistoryChildRoot(parent *os.Root, name string) *os.Root {
	if parent == nil || name == "" || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\\`) || strings.IndexByte(name, 0) >= 0 {
		return nil
	}
	before, err := parent.Lstat(name)
	if !safeHistoryDirectory(before, err) {
		return nil
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil
	}
	return root
}

func safeHistoryDirectory(info os.FileInfo, err error) bool {
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() &&
		info.Mode().Perm()&0o022 == 0
}

// OpenPersistedImageAttachment opens one browser-visible saved image through
// the same symlink- and permission-confined artifact root used by history
// rehydration. The caller owns the returned file. Keeping this resolver shared
// prevents the HTTP attachment endpoint from reintroducing path-following
// behavior that the model-context loader rejects.
func OpenPersistedImageAttachment(chatPath, name string) (*os.File, os.FileInfo, bool) {
	reader := openHistoryImageReader(chatPath)
	if reader == nil {
		return nil, nil, false
	}
	defer reader.root.Close()
	name, fileLimit, ok := historyImageFile(name)
	if !ok {
		return nil, nil, false
	}
	return openVerifiedHistoryImageFile(reader.root, name, fileLimit)
}

func historyImageFile(name string) (string, int64, bool) {
	name = strings.TrimSpace(name)
	if name == "" || !historyImageNamePattern.MatchString(name) || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\\`) || strings.IndexByte(name, 0) >= 0 {
		return "", 0, false
	}
	fileLimit := maxDirectHistoryImageBytes
	if strings.HasPrefix(name, "pdf-page-") {
		fileLimit = maxHistoryImageBytes
	}
	return name, fileLimit, true
}

func openVerifiedHistoryImageFile(root *os.Root, name string, fileLimit int64) (*os.File, os.FileInfo, bool) {
	if root == nil || fileLimit <= 0 {
		return nil, nil, false
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm()&0o022 != 0 || before.Size() <= 0 || before.Size() > fileLimit {
		return nil, nil, false
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, false
	}
	after, err := file.Stat()
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		after.Mode().Perm()&0o022 != 0 || after.Size() != before.Size() || !os.SameFile(before, after) {
		file.Close()
		return nil, nil, false
	}
	return file, after, true
}

func (reader *historyImageReader) read(name string, remainingBytes, remainingDecodedBytes int64) historyImage {
	name, fileLimit, validName := historyImageFile(name)
	if !validName {
		return historyImage{}
	}
	if cached, ok := reader.cache[name]; ok {
		if cached.bytes <= remainingBytes && cached.decodedBytes <= remainingDecodedBytes {
			return cached
		}
		return historyImage{}
	}
	result := historyImage{}
	defer func() { reader.cache[name] = result }()
	if remainingBytes < fileLimit {
		fileLimit = remainingBytes
	}
	file, info, ok := openVerifiedHistoryImageFile(reader.root, name, fileLimit)
	if !ok {
		return result
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, fileLimit+1))
	if err != nil || int64(len(data)) != info.Size() || int64(len(data)) > fileLimit {
		return result
	}
	mimeType, decodedBytes, ok := validatedHistoryImage(data, remainingDecodedBytes)
	if !ok || !historyImageExtensionMatches(name, mimeType) {
		return result
	}
	result = historyImage{
		part: provider.MessageContentPart{
			Type:           "image_url",
			AttachmentKind: provider.AttachmentKindPersistedHistory,
			ImageURL: &provider.MessageImageURL{
				URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
			},
		},
		bytes: int64(len(data)), decodedBytes: decodedBytes, ok: true,
	}
	return result
}

func validatedHistoryImage(data []byte, remainingDecodedBytes int64) (string, int64, bool) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", 0, false
	}
	mimeType := ""
	switch format {
	case "jpeg":
		mimeType = "image/jpeg"
	case "png":
		mimeType = "image/png"
	default:
		return "", 0, false
	}
	decodedBytes, ok := historyImageDecodedBytes(config.Width, config.Height)
	if !ok || decodedBytes > remainingDecodedBytes {
		return "", 0, false
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		return "", 0, false
	}
	decodedBounds := decoded.Bounds()
	if decodedBounds.Dx() != config.Width || decodedBounds.Dy() != config.Height {
		return "", 0, false
	}
	return mimeType, decodedBytes, true
}

func historyImageDecodedBytes(width, height int) (int64, bool) {
	if width <= 0 || height <= 0 || width > maxHistoryImageDimension ||
		height > maxHistoryImageDimension {
		return 0, false
	}
	pixels := int64(width) * int64(height)
	if pixels <= 0 || pixels > maxHistoryImagePixels {
		return 0, false
	}
	return pixels * maxHistoryImageBytesPerPixel, true
}

func historyImageExtensionMatches(name, mimeType string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	switch mimeType {
	case "image/jpeg":
		return extension == ".jpg" || extension == ".jpeg"
	case "image/png":
		return extension == ".png"
	default:
		return false
	}
}
