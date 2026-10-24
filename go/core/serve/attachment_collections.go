package serve

// Original uploads live outside model context. A task carries one sealed,
// chat-owned collection reference; tools page metadata and materialize pixels
// only at an explicit, bounded model boundary.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const attachmentOriginalMaxBytes int64 = 20 << 20
const attachmentStorageMaxBytes int64 = 1 << 30
const attachmentMetadataMaxBytes int64 = 64 << 10

// Serialize disk transactions across live sessions resuming the same chat.
// No image bytes or per-image catalog are retained in this process-wide lock.
var attachmentCollectionMu sync.Mutex

type attachmentCollection struct {
	ID     string `json:"collection_id"`
	Count  int    `json:"count"`
	Bytes  int64  `json:"bytes"`
	Sealed bool   `json:"sealed"`
}

func validAttachmentID(id string) bool {
	return len(id) == 36 && validPromptRequestID(id) && !strings.ContainsAny(id, `/\\`)
}

func privateAttachmentChild(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	before, err := parent.Lstat(name)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("attachment storage must be a private real directory")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := child.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		child.Close()
		return nil, fmt.Errorf("attachment directory changed")
	}
	return child, nil
}

func (sess *serveSession) attachmentRoot(create bool) (*os.Root, error) {
	dir, err := session.ArtifactsDir(sess.cwd, queueChatID(sess))
	if err != nil {
		return nil, err
	}
	// Sibling storage stays outside the model-mounted /session artifacts root.
	// Only collection tools can resolve these opaque references.
	artifacts, err := openArtifactRoot(filepath.Dir(dir), true)
	if err != nil {
		return nil, err
	}
	defer artifacts.Close()
	return privateAttachmentChild(artifacts, "image-collections", create)
}

func (sess *serveSession) openAttachmentCollection(id string, create bool) (*os.Root, error) {
	if !validAttachmentID(id) {
		return nil, fmt.Errorf("invalid attachment collection reference")
	}
	root, err := sess.attachmentRoot(create)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return privateAttachmentChild(root, id, create)
}

func readAttachmentFile(root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o077 != 0 || before.Size() > limit {
		return nil, fmt.Errorf("attachment file unavailable or unsafe")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, fmt.Errorf("attachment file changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit || int64(len(data)) != after.Size() {
		return nil, fmt.Errorf("attachment file exceeded its bound or changed")
	}
	return data, nil
}

func writeAttachmentJSON(root *os.Root, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(data)) > attachmentMetadataMaxBytes {
		return fmt.Errorf("attachment metadata is too large")
	}
	return writeAttachmentAtomic(root, name, data)
}

func writeAttachmentAtomic(root *os.Root, name string, data []byte) error {
	tmp := ".upload-" + session.NewUUID()
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = root.Rename(tmp, name); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readCollection(root *os.Root, id string) (attachmentCollection, error) {
	var value attachmentCollection
	data, err := readAttachmentFile(root, "collection.json", attachmentMetadataMaxBytes)
	if err == nil {
		err = json.Unmarshal(data, &value)
	}
	if err != nil || value.ID != id || value.Count < 0 || value.Bytes < 0 {
		return value, fmt.Errorf("attachment collection unavailable")
	}
	// A single durable journal makes original/id/index/header a recoverable
	// transaction. Never acknowledge an image receipt outside the header's set.
	if _, pendingErr := root.Lstat("pending.json"); errors.Is(pendingErr, os.ErrNotExist) {
		if _, orphanErr := root.Lstat(imageRecordName(value.Count)); !errors.Is(orphanErr, os.ErrNotExist) {
			return value, fmt.Errorf("attachment index has an uncommitted member")
		}
		return value, nil
	} else if pendingErr != nil {
		return value, pendingErr
	}
	journal, err := readAttachmentFile(root, "pending.json", attachmentMetadataMaxBytes)
	var item tools.AttachmentImage
	if err != nil || json.Unmarshal(journal, &item) != nil || item.CollectionID != id || !validAttachmentID(item.ID) || item.Index < 0 || item.Size <= 0 || item.Size > attachmentOriginalMaxBytes {
		return value, fmt.Errorf("attachment transaction journal is unavailable")
	}
	original, err := readAttachmentFile(root, item.ID+".original", attachmentOriginalMaxBytes)
	if err != nil || int64(len(original)) != item.Size || fmt.Sprintf("%x", sha256.Sum256(original)) != item.SHA256 {
		return value, fmt.Errorf("pending original failed integrity verification")
	}
	if value.Count != item.Index && value.Count != item.Index+1 {
		return value, fmt.Errorf("attachment transaction index does not match collection")
	}
	if existing, existingErr := readAttachmentFile(root, imageRecordName(item.Index), attachmentMetadataMaxBytes); existingErr == nil {
		var old tools.AttachmentImage
		if json.Unmarshal(existing, &old) != nil || old.ID != item.ID || old.SHA256 != item.SHA256 {
			return value, fmt.Errorf("attachment transaction would replace another original")
		}
	}
	if err = writeAttachmentJSON(root, item.ID+".json", item); err == nil {
		err = writeAttachmentJSON(root, imageRecordName(item.Index), item)
	}
	if err != nil {
		return value, err
	}
	if value.Count == item.Index {
		value.Count++
		value.Bytes += item.Size
		if err = writeAttachmentJSON(root, "collection.json", value); err != nil {
			return value, err
		}
	}
	if err = root.Remove("pending.json"); err != nil {
		return value, err
	}
	dir, err := root.Open(".")
	if err != nil {
		return value, err
	}
	defer dir.Close()
	return value, dir.Sync()
}

func (s *Server) handleAttachmentCollectionCreate(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, 404, "unknown session")
		return
	}
	var body struct {
		ID string `json:"collection_id"`
	}
	if err := decodeBoundedJSONBody(w, r, attachmentMetadataMaxBytes, &body); err != nil || !validAttachmentID(body.ID) {
		writeCodedErr(w, 400, "invalid_attachment_collection", "collection_id must be a UUID")
		return
	}
	attachmentCollectionMu.Lock()
	defer attachmentCollectionMu.Unlock()
	root, err := sess.openAttachmentCollection(body.ID, true)
	if err != nil {
		writeCodedErr(w, 507, "attachment_storage_unavailable", err.Error())
		return
	}
	defer root.Close()
	value, err := readCollection(root, body.ID)
	if err != nil {
		if _, statErr := root.Lstat("collection.json"); !errors.Is(statErr, os.ErrNotExist) {
			writeErr(w, 409, "existing collection is unavailable")
			return
		}
		value = attachmentCollection{ID: body.ID}
		if err := writeAttachmentJSON(root, "collection.json", value); err != nil {
			writeCodedErr(w, 507, "attachment_storage_unavailable", err.Error())
			return
		}
	}
	writeJSON(w, value)
}

func validateAttachmentOriginal(data []byte, declared string) (string, int, int, error) {
	if len(data) >= 20 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		// Preserve a complete WebP container even when this build has no WebP
		// pixel decoder. The inspection tool reports unsupported_format per item;
		// retaining originals must not require changing model/provider packages.
		width, height, err := attachmentWebPDimensions(data)
		if err != nil || (declared != "" && declared != "image/webp") {
			return "", 0, 0, fmt.Errorf("WebP container or declared media type is invalid")
		}
		if uint64(width)*uint64(height) > (64<<20)/8 {
			return "", 0, 0, fmt.Errorf("image exceeds decoded pixel memory budget")
		}
		return "image/webp", width, height, nil
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return "", 0, 0, fmt.Errorf("image is unsupported or has an invalid header")
	}
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}[format]
	if mime == "" || (declared != "" && declared != mime) {
		return "", 0, 0, fmt.Errorf("image media type does not match validated bytes")
	}
	if uint64(config.Width)*uint64(config.Height) > (64<<20)/8 {
		return "", 0, 0, fmt.Errorf("image exceeds decoded pixel memory budget")
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return "", 0, 0, fmt.Errorf("image content is incomplete or invalid")
	}
	return mime, config.Width, config.Height, nil
}

func attachmentWebPDimensions(data []byte) (int, int, error) {
	if uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return 0, 0, fmt.Errorf("incomplete WebP container")
	}
	width, height, seen := 0, 0, false
	for offset := 12; offset < len(data); {
		if offset+8 > len(data) {
			return 0, 0, fmt.Errorf("incomplete WebP chunk")
		}
		size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		end := uint64(offset) + 8 + size
		if end > uint64(len(data)) {
			return 0, 0, fmt.Errorf("incomplete WebP payload")
		}
		payload := data[offset+8 : int(end)]
		switch string(data[offset : offset+4]) {
		case "VP8 ":
			if len(payload) >= 10 && bytes.Equal(payload[3:6], []byte{0x9d, 0x01, 0x2a}) {
				width = int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
				height = int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
				seen = true
			}
		case "VP8L":
			if len(payload) >= 5 && payload[0] == 0x2f {
				bits := binary.LittleEndian.Uint32(payload[1:5])
				width = int(bits&0x3fff) + 1
				height = int((bits>>14)&0x3fff) + 1
				seen = true
			}
		case "VP8X":
			if len(payload) == 10 {
				width = int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16
				height = int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16
				width++
				height++
			}
		case "ANMF":
			seen = seen || len(payload) >= 16
		}
		offset = int(end) + (int(size) & 1)
		if offset > len(data) {
			return 0, 0, fmt.Errorf("missing WebP chunk padding")
		}
	}
	if !seen || width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("WebP dimensions unavailable")
	}
	return width, height, nil
}

// Count bytes on disk, including an interrupted transaction's orphaned source.
// Directory pages stay bounded; storage capacity never becomes an item limit.
func attachmentDiskBytes(root *os.Root) (int64, error) {
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	var total int64
	for {
		entries, err := dir.ReadDir(64)
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				return 0, fmt.Errorf("attachment storage contains a symlink")
			}
			if e.IsDir() {
				child, childErr := privateAttachmentChild(root, e.Name(), false)
				if childErr != nil {
					return 0, childErr
				}
				n, childErr := attachmentDiskBytes(child)
				child.Close()
				if childErr != nil {
					return 0, childErr
				}
				total += n
			} else {
				info, statErr := e.Info()
				if statErr != nil {
					return 0, statErr
				}
				total += info.Size()
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func imageRecordName(index int) string { return fmt.Sprintf("image-%012d.json", index) }

func (s *Server) handleAttachmentImageUpload(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, 404, "unknown session")
		return
	}
	collectionID, imageID := r.PathValue("collection"), r.PathValue("image")
	if !validAttachmentID(imageID) || !validAttachmentID(collectionID) {
		writeErr(w, 400, "invalid image reference")
		return
	}
	if r.ContentLength > attachmentOriginalMaxBytes {
		writeCodedErr(w, 413, "attachment_file_too_large", "image exceeds 20 MiB")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, attachmentOriginalMaxBytes))
	if err != nil || len(data) == 0 {
		writeCodedErr(w, 413, "attachment_file_invalid", "image is empty, interrupted or exceeds 20 MiB")
		return
	}
	mime, width, height, err := validateAttachmentOriginal(data, strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if err != nil {
		writeCodedErr(w, 422, "attachment_image_invalid", err.Error())
		return
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if wanted := r.Header.Get("X-Meadow-Attachment-SHA256"); wanted != "" && wanted != digest {
		writeCodedErr(w, 409, "attachment_hash_mismatch", "original image hash changed")
		return
	}
	name, err := url.QueryUnescape(r.Header.Get("X-Meadow-Attachment-Name"))
	if err != nil || len(name) > 1024 {
		writeErr(w, 400, "invalid attachment name")
		return
	}
	name = safePDFName(name)
	attachmentCollectionMu.Lock()
	defer attachmentCollectionMu.Unlock()
	root, err := sess.openAttachmentCollection(collectionID, false)
	if err != nil {
		writeErr(w, 404, "unknown attachment collection")
		return
	}
	defer root.Close()
	collection, err := readCollection(root, collectionID)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	// Stable ID receipts let a deliberate retry survive a lost upload response.
	if receiptData, receiptErr := readAttachmentFile(root, imageID+".json", attachmentMetadataMaxBytes); receiptErr == nil {
		var existing tools.AttachmentImage
		if json.Unmarshal(receiptData, &existing) == nil && existing.SHA256 == digest && existing.Size == int64(len(data)) && existing.Name == name {
			original, originalErr := readAttachmentFile(root, imageID+".original", attachmentOriginalMaxBytes)
			if existing.Index < 0 || existing.Index >= collection.Count || originalErr != nil || fmt.Sprintf("%x", sha256.Sum256(original)) != digest {
				writeErr(w, 409, "image receipt is not a committed original")
				return
			}
			writeJSON(w, existing)
			return
		}
		writeErr(w, 409, "image reference already contains a different original")
		return
	}
	if collection.Sealed {
		writeCodedErr(w, 409, "attachment_collection_sealed", "task attachment collection is immutable")
		return
	}
	all, err := sess.attachmentRoot(false)
	if err != nil {
		writeErr(w, 507, "attachment storage unavailable")
		return
	}
	used, err := attachmentDiskBytes(all)
	all.Close()
	if err != nil || used+int64(len(data))+3*attachmentMetadataMaxBytes > attachmentStorageMaxBytes {
		writeCodedErr(w, 507, "attachment_capacity_exhausted", "private image storage capacity is exhausted; originals remain retained")
		return
	}
	item := tools.AttachmentImage{ID: imageID, CollectionID: collectionID, Name: name, MIME: mime, SHA256: digest, Size: int64(len(data)), Width: width, Height: height, Index: collection.Count, State: "uploaded"}
	if err = writeAttachmentAtomic(root, imageID+".original", data); err == nil {
		err = writeAttachmentJSON(root, "pending.json", item)
	}
	if err != nil {
		writeCodedErr(w, 507, "attachment_storage_unavailable", "original image could not be committed")
		return
	}
	if _, err = readCollection(root, collectionID); err != nil {
		writeCodedErr(w, 507, "attachment_commit_uncertain", "original saved but collection commit is uncertain; retry this image identity")
		return
	}
	sess.broadcast("attachment_progress", map[string]any{"collection_id": collectionID, "image": item})
	writeJSON(w, item)
}

type sessionAttachmentSource struct{ session *serveSession }

func (source *sessionAttachmentSource) VisionAvailable() bool {
	source.session.mu.Lock()
	if source.session.engine == nil {
		source.session.mu.Unlock()
		return false
	}
	client, model := source.session.engine.Client, source.session.engine.ModelID
	source.session.mu.Unlock()
	profile, known := provider.ProfileForClient(client, model)
	return known && profile.Multimodal
}

func (source *sessionAttachmentSource) ListImages(ctx context.Context, collectionID, cursor string, limit int) (tools.AttachmentImagePage, error) {
	page := tools.AttachmentImagePage{CollectionID: collectionID, Items: []tools.AttachmentImage{}}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil || start < 0 {
			return page, fmt.Errorf("invalid attachment cursor")
		}
	}
	if limit < 1 || limit > 100 {
		return page, fmt.Errorf("attachment page limit must be 1-100")
	}
	attachmentCollectionMu.Lock()
	defer attachmentCollectionMu.Unlock()
	root, err := source.session.openAttachmentCollection(collectionID, false)
	if err != nil {
		return page, err
	}
	defer root.Close()
	collection, err := readCollection(root, collectionID)
	if err != nil {
		return page, err
	}
	page.Total = collection.Count
	if start > collection.Count {
		return page, fmt.Errorf("attachment cursor is outside collection")
	}
	for index := start; index < min(start+limit, collection.Count); index++ {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		data, err := readAttachmentFile(root, imageRecordName(index), attachmentMetadataMaxBytes)
		var item tools.AttachmentImage
		if err != nil || json.Unmarshal(data, &item) != nil || item.CollectionID != collectionID || item.Index != index || !validAttachmentID(item.ID) {
			return page, fmt.Errorf("image metadata at index %d unavailable", index)
		}
		page.Items = append(page.Items, item)
	}
	if next := start + len(page.Items); next < collection.Count {
		page.NextCursor = strconv.Itoa(next)
	}
	return page, nil
}

func (source *sessionAttachmentSource) ReadImage(ctx context.Context, collectionID, imageID string) (tools.AttachmentImage, []byte, error) {
	var item tools.AttachmentImage
	if err := ctx.Err(); err != nil {
		return item, nil, err
	}
	if !validAttachmentID(imageID) {
		return item, nil, fmt.Errorf("invalid image reference")
	}
	attachmentCollectionMu.Lock()
	defer attachmentCollectionMu.Unlock()
	root, err := source.session.openAttachmentCollection(collectionID, false)
	if err != nil {
		return item, nil, err
	}
	defer root.Close()
	meta, err := readAttachmentFile(root, imageID+".json", attachmentMetadataMaxBytes)
	if err != nil || json.Unmarshal(meta, &item) != nil || item.ID != imageID || item.CollectionID != collectionID {
		return item, nil, fmt.Errorf("image reference is not in this chat collection")
	}
	data, err := readAttachmentFile(root, imageID+".original", attachmentOriginalMaxBytes)
	if err != nil {
		return item, nil, err
	}
	if int64(len(data)) != item.Size || fmt.Sprintf("%x", sha256.Sum256(data)) != item.SHA256 {
		return item, nil, fmt.Errorf("original image integrity check failed")
	}
	return item, data, nil
}

func (source *sessionAttachmentSource) NoteImageProgress(ctx context.Context, collectionID, imageID, state, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validAttachmentID(imageID) {
		return fmt.Errorf("invalid image reference")
	}
	attachmentCollectionMu.Lock()
	defer attachmentCollectionMu.Unlock()
	root, err := source.session.openAttachmentCollection(collectionID, false)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := readAttachmentFile(root, imageID+".json", attachmentMetadataMaxBytes)
	var item tools.AttachmentImage
	if err != nil || json.Unmarshal(data, &item) != nil || item.ID != imageID || item.CollectionID != collectionID {
		return fmt.Errorf("image progress unavailable")
	}
	item.State, item.Error = state, ""
	if state == "failed" {
		item.Error = message
	}
	if err = writeAttachmentJSON(root, imageID+".json", item); err == nil {
		err = writeAttachmentJSON(root, imageRecordName(item.Index), item)
	}
	if err == nil {
		source.session.broadcast("attachment_progress", map[string]any{"collection_id": collectionID, "image": item})
	}
	return err
}

func (s *Server) handleAttachmentCollectionList(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, 404, "unknown session")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeErr(w, 400, "invalid page limit")
			return
		}
	}
	page, err := (&sessionAttachmentSource{sess}).ListImages(r.Context(), r.PathValue("collection"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	writeJSON(w, page)
}

func (sess *serveSession) bindAttachmentImageTools() {
	if sess.engine != nil && sess.engine.Registry != nil {
		tools.RegisterAttachmentImageTools(sess.engine.Registry, &sessionAttachmentSource{sess})
	}
}

// Background observations are durable model context and deliberately do not
// create a second human bubble in history. Keep the original user text exact.
func persistAttachmentCollectionContext(writer *session.ChatWriter, raw []map[string]any) error {
	for _, block := range raw {
		if stringValue(block["type"]) == "attachment_collection" {
			if summary := stringValue(block["_attachment_summary"]); summary != "" {
				if err := writer.BackgroundObservation(summary); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Derive collection instructions from the protected disk header. Ignore any
// summary/count supplied by a caller. Sealing freezes this task's original set.
func (sess *serveSession) prepareAttachmentCollections(raw []map[string]any) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(raw))
	for _, block := range raw {
		if stringValue(block["type"]) != "attachment_collection" {
			result = append(result, block)
			continue
		}
		id := stringValue(block["collection_id"])
		attachmentCollectionMu.Lock()
		root, err := sess.openAttachmentCollection(id, false)
		var collection attachmentCollection
		if err == nil {
			collection, err = readCollection(root, id)
			if err == nil && collection.Count == 0 {
				err = fmt.Errorf("attachment collection is empty")
			}
			if err == nil && block["expected_count"] != nil && intValue(block["expected_count"]) != collection.Count {
				err = fmt.Errorf("attachment collection membership changed; use a new message identity")
			}
			if wanted := stringValue(block["manifest_sha256"]); err == nil && wanted != "" {
				hash := sha256.New()
				for index := 0; index < collection.Count; index++ {
					data, readErr := readAttachmentFile(root, imageRecordName(index), attachmentMetadataMaxBytes)
					var item tools.AttachmentImage
					if readErr != nil || json.Unmarshal(data, &item) != nil || item.CollectionID != id || item.Index != index {
						err = fmt.Errorf("attachment membership metadata unavailable")
						break
					}
					fmt.Fprintf(hash, "%s\x00%s\n", item.ID, item.SHA256)
				}
				if err == nil && wanted != fmt.Sprintf("%x", hash.Sum(nil)) {
					err = fmt.Errorf("attachment collection order or sources changed; use a new message identity")
				}
			}
			if err == nil && !collection.Sealed {
				collection.Sealed = true
				err = writeAttachmentJSON(root, "collection.json", collection)
			}
			root.Close()
		}
		attachmentCollectionMu.Unlock()
		if err != nil {
			return nil, err
		}
		capability := "The active model supports vision. Use list_attachment_images and inspect_attachment_images to inspect all relevant images in bounded batches; pixels have not yet been supplied."
		if !(&sessionAttachmentSource{sess}).VisionAvailable() {
			capability = "The active model does not have verified vision capability. Image originals are retained, but their visual contents have NOT been inspected. Do not claim visual understanding; inspect_attachment_images will report vision_unavailable. A vision model must be explicitly selected before pixel inspection."
		}
		summary := fmt.Sprintf("ATTACHMENT_COLLECTION %s: %d original images, %d bytes retained in private chat storage. Source metadata and per-image progress are available through list_attachment_images (paginated). %s", id, collection.Count, collection.Bytes, capability)
		result = append(result, map[string]any{"type": "attachment_collection", "collection_id": id, "_attachment_summary": summary, "count": collection.Count, "expected_count": collection.Count, "manifest_sha256": stringValue(block["manifest_sha256"])})
	}
	return result, nil
}
