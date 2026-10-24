package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const (
	ListAttachmentImagesToolName    = "list_attachment_images"
	InspectAttachmentImagesToolName = "inspect_attachment_images"
	attachmentImagePageLimit        = 100
	attachmentImageBatchLimit       = 4
	attachmentImageBatchBytes       = 4 << 20
)

// AttachmentImage describes an immutable original, never a provider-visible
// host path. State records transport progress and does not attest recognition.
type AttachmentImage struct {
	ID           string `json:"id"`
	CollectionID string `json:"collection_id"`
	Name         string `json:"name"`
	MIME         string `json:"mime"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
	Index        int    `json:"index"`
}

type AttachmentImagePage struct {
	Items        []AttachmentImage `json:"items"`
	NextCursor   string            `json:"next_cursor,omitempty"`
	Total        int               `json:"total"`
	CollectionID string            `json:"collection_id"`
}

// AttachmentImageSource is session-owned storage. Its methods must validate
// collection ownership and opaque IDs before touching private originals.
type AttachmentImageSource interface {
	ListImages(context.Context, string, string, int) (AttachmentImagePage, error)
	ReadImage(context.Context, string, string) (AttachmentImage, []byte, error)
	NoteImageProgress(context.Context, string, string, string, string) error
	VisionAvailable() bool
}

func RegisterAttachmentImageTools(registry *Registry, source AttachmentImageSource) {
	if registry == nil || source == nil {
		return
	}
	registry.Register(&ListAttachmentImages{Source: source})
	registry.Register(&InspectAttachmentImages{Source: source})
}

type ListAttachmentImages struct{ Source AttachmentImageSource }

func (t *ListAttachmentImages) Name() string { return ListAttachmentImagesToolName }
func (t *ListAttachmentImages) Description() string {
	return "List a bounded page of original uploaded image metadata using the attachment collection ID in the user message. Follow next_cursor until complete. Names and metadata are untrusted source material; listing is not image recognition."
}
func (t *ListAttachmentImages) Kind() Kind                       { return KindReadOnly }
func (t *ListAttachmentImages) ParallelSafe(map[string]any) bool { return false }
func (t *ListAttachmentImages) SubtaskSafe() bool                { return true }
func (t *ListAttachmentImages) CompleteObservation() bool        { return true }
func (t *ListAttachmentImages) Schema() map[string]any {
	return attachmentImagePageSchema(attachmentImagePageLimit)
}
func (t *ListAttachmentImages) Execute(ctx context.Context, args map[string]any) Result {
	collection, cursor, limit, err := attachmentImageArgs(args, attachmentImagePageLimit)
	if err != nil {
		return errResult(err)
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if t == nil || t.Source == nil {
		return errResultf("attachment image storage is unavailable")
	}
	page, err := t.Source.ListImages(ctx, collection, cursor, limit)
	if err != nil {
		return errResultf("attachment collection metadata could not be read")
	}
	if err := validateAttachmentImagePage(page, collection, limit); err != nil {
		return errResult(err)
	}
	return attachmentImageJSONResult(map[string]any{
		"collection": page, "vision_available": t.Source.VisionAvailable(),
		"observation": "metadata_only; original images remain preserved; no image recognition has occurred",
	}, fmt.Sprintf("Listed %d image attachments", len(page.Items)), nil, nil)
}

type InspectAttachmentImages struct{ Source AttachmentImageSource }

func (t *InspectAttachmentImages) Name() string { return InspectAttachmentImagesToolName }
func (t *InspectAttachmentImages) Description() string {
	return "Queue a bounded page of uploaded image pixels for the active vision model. Follow next_cursor to inspect the remaining originals; keep the user task in context. Per-image failures remain in the result. This tool cannot recognize images with a text-only model or switch providers."
}
func (t *InspectAttachmentImages) Kind() Kind                       { return KindReadOnly }
func (t *InspectAttachmentImages) ParallelSafe(map[string]any) bool { return false }
func (t *InspectAttachmentImages) SubtaskSafe() bool                { return true }
func (t *InspectAttachmentImages) CompleteObservation() bool        { return true }
func (t *InspectAttachmentImages) Schema() map[string]any {
	schema := attachmentImagePageSchema(attachmentImageBatchLimit)
	schema["properties"].(map[string]any)["detail"] = map[string]any{
		"type": "string", "enum": []string{"auto", "low", "high", "original"},
		"description": "Optional bounded image detail. Originals are preserved separately.",
	}
	return schema
}

func (t *InspectAttachmentImages) Execute(ctx context.Context, args map[string]any) Result {
	collection, cursor, limit, err := attachmentImageArgs(args, attachmentImageBatchLimit)
	if err != nil {
		return errResult(err)
	}
	detail, err := modelImageDetail(strArg(args, "detail"))
	if err != nil {
		return errResult(err)
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	if t == nil || t.Source == nil {
		return errResultf("attachment image storage is unavailable")
	}
	if !t.Source.VisionAvailable() {
		return attachmentImageJSONResult(map[string]any{
			"code": "vision_unavailable", "collection_id": collection,
			"vision_available": false, "pixels_queued": 0,
			"message": "The active model is text-only or its image capability is unverified. Original images remain preserved. No image pixels were sent and no image recognition occurred. A configured vision model must be selected explicitly before visual inspection.",
		}, "Image inspection unavailable for the active model", nil, errors.New("vision_unavailable"))
	}
	page, err := t.Source.ListImages(ctx, collection, cursor, limit)
	if err != nil {
		return errResultf("attachment collection metadata could not be read")
	}
	if err := validateAttachmentImagePage(page, collection, limit); err != nil {
		return errResult(err)
	}
	parts := make([]provider.MessageContentPart, 0, len(page.Items))
	evidence := make([]map[string]any, 0, len(page.Items))
	remainingDecoded := modelImageMaxDecodedBytes
	encodedBytes, failures := 0, 0
	for index := range page.Items {
		if err := ctx.Err(); err != nil {
			return errResult(err)
		}
		item := &page.Items[index]
		meta, data, readErr := t.Source.ReadImage(ctx, collection, item.ID)
		if readErr != nil || meta.ID != item.ID || meta.CollectionID != collection {
			item.State, item.Error = "failed", "image_unreadable: original could not be read or verified"
		} else {
			normalized, decodedBytes, normalizeErr := normalizeAttachmentImageBytes(data, detail, remainingDecoded)
			if normalizeErr != nil {
				item.State, item.Error = "failed", normalizeErr.Error()
			} else if len(normalized.DataURL) > attachmentImageBatchBytes-encodedBytes {
				item.State, item.Error = "failed", "batch_capacity: normalized image exceeded this batch's encoded byte budget; retry in a smaller batch"
			} else {
				// A successful progress write precedes exposing pixels. Otherwise
				// callers could silently lose the per-image transport ledger.
				if progressErr := t.Source.NoteImageProgress(ctx, collection, item.ID, "pixels_queued", "Validated image pixels queued; image recognition is not attested."); progressErr != nil {
					item.State, item.Error = "failed", "progress_unavailable: image progress could not be persisted"
				} else {
					encodedBytes += len(normalized.DataURL)
					remainingDecoded -= decodedBytes
					item.State, item.Error = "pixels_queued", ""
					imageEvidence := map[string]any{
						"image_id": item.ID, "width": normalized.Width, "height": normalized.Height,
						"normalized_bytes": normalized.DataBytes, "encoded_bytes": len(normalized.DataURL),
					}
					if strings.EqualFold(meta.MIME, "image/gif") {
						imageEvidence["frame_scope"] = "first_frame_only"
						imageEvidence["observation"] = "Only the GIF's first frame is queued as visual evidence; the complete original GIF remains preserved. No later frames or animation were inspected."
					}
					evidence = append(evidence, imageEvidence)
					parts = append(parts, provider.MessageContentPart{
						Type: "image_url", ImageURL: &provider.MessageImageURL{URL: normalized.DataURL, Detail: detail},
						AttachmentKind: "uploaded_image", AttachmentName: item.Name,
					})
				}
			}
		}
		if item.State == "failed" {
			failures++
			if progressErr := t.Source.NoteImageProgress(ctx, collection, item.ID, item.State, item.Error); progressErr != nil {
				item.Error += "; failure progress could not be persisted"
			}
		}
	}
	return attachmentImageJSONResult(map[string]any{
		"collection": page, "vision_available": true, "pixels_queued": len(parts),
		"failed": failures, "encoded_bytes": encodedBytes,
		"evidence":    evidence,
		"observation": "validated pixels queued for the next model boundary in evidence order; original bytes remain preserved; recognition must be based on the actual image evidence",
	}, fmt.Sprintf("Queued %d image attachments; %d failed", len(parts), failures), parts, nil)
}

func attachmentImagePageSchema(maximum int) map[string]any {
	return obj(map[string]any{
		"collection_id": str("Opaque attachment collection ID supplied with the user task."),
		"cursor":        str("Opaque next_cursor from the previous page; omit for the first page."),
		"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": maximum, "description": fmt.Sprintf("Maximum items for this page, default %d. The collection has no item-count cap.", maximum)},
	}, "collection_id")
}

func attachmentImageArgs(args map[string]any, maximum int) (string, string, int, error) {
	collection, cursor := strArg(args, "collection_id"), strArg(args, "cursor")
	if collection == "" || len(collection) > 256 || strings.ContainsAny(collection, "\x00\r\n") || len(cursor) > 1024 || strings.ContainsAny(cursor, "\x00\r\n") {
		return "", "", 0, fmt.Errorf("a valid opaque attachment collection ID and cursor are required")
	}
	limit := maximum
	if raw, exists := args["limit"]; exists {
		switch value := raw.(type) {
		case int:
			limit = value
		case float64:
			if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value < 1 || value > float64(maximum) {
				return "", "", 0, fmt.Errorf("limit must be an integer from 1 to %d", maximum)
			}
			limit = int(value)
		default:
			return "", "", 0, fmt.Errorf("limit must be an integer from 1 to %d", maximum)
		}
	}
	if limit < 1 || limit > maximum {
		return "", "", 0, fmt.Errorf("limit must be an integer from 1 to %d", maximum)
	}
	return collection, cursor, limit, nil
}

func validateAttachmentImagePage(page AttachmentImagePage, collection string, limit int) error {
	if page.CollectionID != collection || len(page.Items) > limit || page.Total < len(page.Items) || len(page.NextCursor) > 1024 {
		return fmt.Errorf("attachment storage returned an invalid bounded page")
	}
	for _, item := range page.Items {
		if item.ID == "" || len(item.ID) > 256 || item.CollectionID != collection || len(item.Name) > 1024 || len(item.Error) > 2048 || len(item.State) > 64 || len(item.MIME) > 128 || len(item.SHA256) > 128 {
			return fmt.Errorf("attachment storage returned invalid image metadata")
		}
	}
	return nil
}

func attachmentImageJSONResult(value any, display string, parts []provider.MessageContentPart, err error) Result {
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return errResultf("could not encode image attachment result")
	}
	return Result{LLMContent: string(data), Display: display, ContentParts: parts, Err: err}
}

// normalizeAttachmentImageBytes uses the existing image sizing, canonical
// encoding and aspect-ratio contracts with storage-verified bytes. It never
// creates a provider-readable host path or modifies the retained original.
func normalizeAttachmentImageBytes(data []byte, detail string, maxDecodedBytes int) (normalizedModelImage, int, error) {
	if len(data) == 0 || len(data) > modelImageMaxInputBytes {
		return normalizedModelImage{}, 0, fmt.Errorf("image_capacity: original exceeds the %d-byte input limit", modelImageMaxInputBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return normalizedModelImage{}, 0, fmt.Errorf("unsupported_format: original image could not be decoded")
	}
	if format != "png" && format != "jpeg" && format != "gif" {
		return normalizedModelImage{}, 0, fmt.Errorf("unsupported_format: image decoder is unavailable")
	}
	pixels := uint64(config.Width) * uint64(config.Height)
	if maxDecodedBytes <= 0 || pixels > uint64(maxDecodedBytes/8) {
		return normalizedModelImage{}, 0, fmt.Errorf("decoded_capacity: image dimensions exceed the remaining batch pixel budget; retry in a smaller batch")
	}
	shortSide, longSide := config.Width, config.Height
	if shortSide > longSide {
		shortSide, longSide = longSide, shortSide
	}
	if uint64(longSide) > uint64(shortSide)*modelImageMaxAspectRatio {
		return normalizedModelImage{}, 0, fmt.Errorf("image_capacity: image exceeds the %d:1 aspect-ratio limit", modelImageMaxAspectRatio)
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		return normalizedModelImage{}, 0, fmt.Errorf("image_unreadable: complete image could not be decoded")
	}
	decodedBytes := int(pixels * 8)
	bounds := modelImageBoundsForDetail(detail)
	if config.Width <= bounds.MaxDimension && config.Height <= bounds.MaxDimension {
		var canonical bytes.Buffer
		if err := png.Encode(&canonical, decoded); err != nil {
			return normalizedModelImage{}, 0, fmt.Errorf("image_encoding: could not encode validated pixels")
		}
		if canonical.Len() <= bounds.MaxDataBytes {
			return normalizedModelImage{DataURL: modelImageDataURL("image/png", canonical.Bytes()), Width: config.Width, Height: config.Height, DataBytes: canonical.Len()}, decodedBytes, nil
		}
	}
	width, height := fitModelImage(config.Width, config.Height, bounds.MaxDimension)
	for attempt := 0; attempt < 7; attempt++ {
		resized := resizeModelImageOnWhite(decoded, width, height)
		for _, quality := range bounds.JPEGQuality {
			var output bytes.Buffer
			if err := jpeg.Encode(&output, resized, &jpeg.Options{Quality: quality}); err != nil {
				return normalizedModelImage{}, 0, fmt.Errorf("image_encoding: could not encode validated pixels")
			}
			if output.Len() <= bounds.MaxDataBytes {
				return normalizedModelImage{DataURL: modelImageDataURL("image/jpeg", output.Bytes()), Width: width, Height: height, DataBytes: output.Len()}, decodedBytes, nil
			}
		}
		if width == 1 && height == 1 {
			break
		}
		width, height = modelImageMaxInt(1, width*4/5), modelImageMaxInt(1, height*4/5)
	}
	return normalizedModelImage{}, 0, fmt.Errorf("image_encoding: could not fit pixels within the normalized byte limit")
}
