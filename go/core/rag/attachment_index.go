package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const AttachmentDocumentPrefix = "attachdoc-"

const attachmentCoordinateScope = "source_id logical lines of LF-normalized bounded extraction; use document_read; not physical extract_path sidecar lines or original file/PDF page coordinates"

// AttachmentInput is a trusted post-extraction handoff, never model arguments.
// Text is the bounded UTF8 extraction; paths are immutable uploaded artifacts.
type AttachmentInput struct {
	Name         string `json:"name"`
	MediaType    string `json:"media_type"`
	OriginalPath string `json:"original_path"`
	ExtractPath  string `json:"extract_path"`
	SourceSHA256 string `json:"source_sha256"`
	Text         string `json:"-"`
	Truncated    bool   `json:"extraction_truncated"`
	Units        int    `json:"units"`
	Note         string `json:"note,omitempty"`
}

type AttachmentReceipt struct {
	SourceID            string `json:"source_id"`
	Name                string `json:"name"`
	OriginalPath        string `json:"original_path"`
	ExtractPath         string `json:"extract_path"`
	SourceSHA256        string `json:"source_sha256"`
	TextSHA256          string `json:"text_sha256"`
	ExtractSHA256       string `json:"extract_sha256,omitempty"`
	Lines               int    `json:"lines"`
	Chunks              int    `json:"chunks"`
	IndexedChunks       int    `json:"indexed_chunks"`
	IndexState          string `json:"index_state"`
	ExtractionTruncated bool   `json:"extraction_truncated"`
	CoordinateScope     string `json:"coordinate_scope"`
	Warning             string `json:"warning,omitempty"`
	UnconfirmedWrites   int    `json:"unconfirmed_remote_writes,omitempty"`
}

type AttachmentKnowledgeService interface {
	Identity() string
	Create(context.Context, string) (tools.MeadowKnowledgeCreateResult, error)
	Put(context.Context, string, string, string) (tools.MeadowKnowledgePutResult, error)
	Search(context.Context, string, string, int) (tools.MeadowKnowledgeSearchResult, error)
}

type attachmentRemoteChunk struct {
	ChunkID  string `json:"chunk_id"`
	RecordID string `json:"record_id"`
	Revision int    `json:"revision"`
	Version  string `json:"source_version"`
}

type attachmentRecord struct {
	Input      AttachmentInput         `json:"input"`
	Receipt    AttachmentReceipt       `json:"receipt"`
	DocumentID string                  `json:"document_id"`
	Remote     []attachmentRemoteChunk `json:"remote"`
}

type attachmentManifest struct {
	Schema     int                          `json:"schema"`
	Scope      string                       `json:"scope"`
	Collection string                       `json:"collection,omitempty"`
	Documents  map[string]*attachmentRecord `json:"documents"`
}

type AttachmentIndexManager struct {
	root    string
	service AttachmentKnowledgeService
	mu      sync.Mutex
	indexes map[string]*AttachmentIndex
}

type AttachmentIndex struct {
	gate      chan struct{}
	directory string
	store     *DocumentStore
	primary   *DocumentStore
	service   AttachmentKnowledgeService
	manifest  attachmentManifest
}

func NewAttachmentIndexManager(root string, service AttachmentKnowledgeService) *AttachmentIndexManager {
	return &AttachmentIndexManager{root: root, service: service, indexes: map[string]*AttachmentIndex{}}
}

func (m *AttachmentIndexManager) Session(cwd, chatID string) (*AttachmentIndex, error) {
	if m == nil || strings.TrimSpace(chatID) == "" || !filepath.IsAbs(cwd) {
		return nil, errors.New("attachment index requires a durable chat and absolute workspace")
	}
	identity := "provider-unavailable"
	if m.service != nil {
		identity = m.service.Identity()
	}
	hash := sha256.Sum256([]byte(identity + "\x00" + filepath.Clean(cwd) + "\x00" + chatID))
	scope := hex.EncodeToString(hash[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if index := m.indexes[scope]; index != nil {
		return index, nil
	}
	dir := filepath.Join(m.root, scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create private attachment index: %w", err)
	}
	index := &AttachmentIndex{gate: make(chan struct{}, 1), directory: dir, store: NewDocumentStore(filepath.Join(dir, "documents")), primary: NewDocumentStore(filepath.Join(dir, "pasted-documents")), service: m.service,
		manifest: attachmentManifest{Schema: 1, Scope: scope, Documents: map[string]*attachmentRecord{}}}
	if data, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		if len(data) > 2<<20 || json.Unmarshal(data, &index.manifest) != nil || index.manifest.Schema != 1 || index.manifest.Scope != scope || index.manifest.Documents == nil {
			return nil, errors.New("attachment index manifest is invalid")
		}
		for id, record := range index.manifest.Documents {
			if record == nil {
				return nil, errors.New("attachment index source receipt is invalid")
			}
			doc, ok := index.store.Get(record.DocumentID)
			if !ok || record.Receipt.SourceID != id || !strings.HasPrefix(id, AttachmentDocumentPrefix) || doc.SHA256 != record.Receipt.TextSHA256 {
				return nil, errors.New("attachment index source receipt is invalid")
			}
			record.Receipt.CoordinateScope = attachmentCoordinateScope
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("attachment index manifest unavailable")
	}
	m.indexes[scope] = index
	return index, nil
}

func (index *AttachmentIndex) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case index.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-index.gate
			return nil, err
		}
		return func() { <-index.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (index *AttachmentIndex) persist() error {
	data, err := json.Marshal(index.manifest)
	if err != nil || len(data) > 2<<20 {
		return errors.New("attachment manifest capacity exceeded")
	}
	f, err := os.CreateTemp(index.directory, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(index.directory, "manifest.json"))
}

func validateAttachmentInput(input AttachmentInput) error {
	if len(input.Name) > 1024 || input.Name == "" || !utf8.ValidString(input.Text) || utf8.RuneCountInString(input.Text) > 256000 || strings.TrimSpace(input.Text) == "" {
		return errors.New("attachment extraction is empty, invalid or exceeds 256000 characters")
	}
	if len(input.SourceSHA256) != 64 {
		return errors.New("attachment source hash is missing")
	}
	if _, err := hex.DecodeString(input.SourceSHA256); err != nil {
		return errors.New("attachment source hash is invalid")
	}
	for _, path := range []string{input.OriginalPath, input.ExtractPath} {
		if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("attachment source coordinates require immutable absolute paths")
		}
	}
	return nil
}

// Ingest is bounded across the whole upload and serializes acknowledged remote
// writes per scope. Local source reads remain available during provider failure.
func (index *AttachmentIndex) Ingest(ctx context.Context, inputs []AttachmentInput) ([]AttachmentReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	release, err := index.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	receipts := make([]AttachmentReceipt, 0, len(inputs))
	for _, input := range inputs {
		if err := validateAttachmentInput(input); err != nil {
			return receipts, err
		}
		text := NormalizeDocumentText(input.Text)
		textHash := sha256.Sum256([]byte(text))
		hash := sha256.Sum256([]byte(index.manifest.Scope + "\x00" + input.OriginalPath + "\x00" + input.SourceSHA256 + "\x00" + hex.EncodeToString(textHash[:])))
		id := AttachmentDocumentPrefix + hex.EncodeToString(hash[:])
		record := index.manifest.Documents[id]
		if record == nil {
			bytes := len(text)
			for _, known := range index.manifest.Documents {
				if doc, ok := index.store.Get(known.DocumentID); ok {
					bytes += doc.Bytes
				}
			}
			if len(index.manifest.Documents) >= 128 || bytes > 32<<20 {
				return receipts, errors.New("session attachment capacity reached")
			}
		}
		doc, err := index.store.Ingest(ctx, input.Name, text, nil)
		if err != nil {
			return receipts, err
		}
		if record == nil {
			if len(index.manifest.Documents) >= 128 {
				return receipts, errors.New("session attachment capacity reached")
			}
			// Bind the exact server-created readable sidecar, including its receipt
			// header and long-line continuation markers. Its bytes intentionally
			// differ from the canonical indexed extraction. Missing/legacy sidecars
			// stay unbound and cannot pass source comparison.
			_, extractHash, bound := comparisonFile(ctx, input.ExtractPath, 8<<20, false)
			if !bound {
				extractHash = ""
			}
			input.Text = ""
			record = &attachmentRecord{Input: input, DocumentID: doc.ID, Receipt: AttachmentReceipt{SourceID: id, Name: input.Name, OriginalPath: input.OriginalPath, ExtractPath: input.ExtractPath, SourceSHA256: input.SourceSHA256, TextSHA256: doc.SHA256, ExtractSHA256: extractHash, Lines: doc.Lines, Chunks: len(doc.Chunks), IndexState: "local_only", ExtractionTruncated: input.Truncated, CoordinateScope: attachmentCoordinateScope}}
			index.manifest.Documents[id] = record
			if err := index.persist(); err != nil {
				return receipts, err
			}
		}
		record.Receipt.Warning = ""
		if index.service == nil {
			record.Receipt.Warning = "Dense retrieval unavailable; exact local source and lexical search remain available."
		} else {
			if index.manifest.Collection == "" {
				created, createErr := index.service.Create(ctx, "Native chat attachments "+index.manifest.Scope[:12])
				if createErr != nil {
					record.Receipt.Warning = "Private retrieval service unavailable; local source preserved."
					var status *tools.MeadowKnowledgeHTTPError
					if !errors.As(createErr, &status) {
						record.Receipt.UnconfirmedWrites++
					}
				} else {
					index.manifest.Collection = created.ID
					if err = index.persist(); err != nil {
						return receipts, err
					}
				}
			}
			if index.manifest.Collection != "" {
				known := map[string]bool{}
				for _, chunk := range record.Remote {
					known[chunk.ChunkID] = true
				}
				for _, chunk := range doc.Chunks {
					if known[chunk.ID] {
						continue
					}
					if len(chunk.Text) > 16000 || utf8.RuneCountInString(chunk.Text) > 8000 {
						record.Receipt.Warning = "Remote chunk limit exceeded; exact local source preserved."
						break
					}
					put, putErr := index.service.Put(ctx, index.manifest.Collection, boundedAttachmentTitle(input.Name), chunk.Text)
					if putErr != nil {
						record.Receipt.Warning = "Hybrid indexing incomplete; acknowledged chunks retained for retry, local source preserved."
						var status *tools.MeadowKnowledgeHTTPError
						if !errors.As(putErr, &status) {
							record.Receipt.UnconfirmedWrites++
						}
						break
					}
					if put.Collection != index.manifest.Collection || put.Published || put.ID == "" || put.SourceVersion == "" {
						record.Receipt.Warning = "Invalid private indexing receipt; local source preserved."
						break
					}
					record.Remote = append(record.Remote, attachmentRemoteChunk{ChunkID: chunk.ID, RecordID: put.ID, Revision: put.Revision, Version: put.SourceVersion})
					if err = index.persist(); err != nil {
						return receipts, err
					}
				}
			}
		}
		record.Receipt.IndexedChunks = len(record.Remote)
		if len(record.Remote) == len(doc.Chunks) {
			record.Receipt.IndexState = "ready"
		} else if len(record.Remote) > 0 {
			record.Receipt.IndexState = "partial"
		} else {
			record.Receipt.IndexState = "local_only"
		}
		if record.Receipt.UnconfirmedWrites > 0 {
			record.Receipt.Warning += " Remote write acknowledgments were unconfirmed; only registered receipts participate in retrieval."
		}
		if err = index.persist(); err != nil {
			return receipts, err
		}
		receipts = append(receipts, record.Receipt)
		if ctx.Err() != nil {
			return receipts, ctx.Err()
		}
	}
	return receipts, nil
}

// PrimaryStore scopes ordinary large pasted inputs to this same native chat.
// It is separate from uploaded sources and the old process-wide collection.
func (index *AttachmentIndex) PrimaryStore() *DocumentStore {
	if index == nil {
		return nil
	}
	return index.primary
}

func boundedAttachmentTitle(text string) string {
	r := []rune(text)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

func (index *AttachmentIndex) Get(ctx context.Context, id string) (*Document, AttachmentReceipt, bool) {
	if index == nil {
		return nil, AttachmentReceipt{}, false
	}
	release, err := index.acquire(ctx)
	if err != nil {
		return nil, AttachmentReceipt{}, false
	}
	defer release()
	record := index.manifest.Documents[id]
	if record == nil {
		return nil, AttachmentReceipt{}, false
	}
	doc, ok := index.store.Get(record.DocumentID)
	if !ok {
		return nil, AttachmentReceipt{}, false
	}
	doc.ID = id
	for i := range doc.Chunks {
		doc.Chunks[i].ID = strings.Replace(doc.Chunks[i].ID, record.DocumentID, id, 1)
	}
	return doc, record.Receipt, true
}

func attachmentExcerpt(receipt AttachmentReceipt, excerpt string) string {
	data, _ := json.Marshal(receipt)
	return "attachment_source: " + string(data) + "\n" + excerpt
}
