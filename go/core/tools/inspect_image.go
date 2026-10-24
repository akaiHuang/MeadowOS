package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hey-meadow/meadow-code/go/core/provider"
)

const InspectImageToolName = "inspect_image"

const (
	modelImageMaxInputBytes   = 20 << 20
	modelImageMaxDecodedBytes = 64 << 20
	modelImageMaxAspectRatio  = 64
)

type normalizedModelImage struct {
	Name      string
	DataURL   string
	Width     int
	Height    int
	DataBytes int
}

type modelImageBounds struct {
	MaxDimension int
	MaxDataBytes int
	JPEGQuality  []int
}

// InspectImage is the explicit local-pixel counterpart to ReadFile. Keeping a
// separate tool preserves read_file's UTF-8-only contract while allowing a
// multimodal provider to receive validated PNG/JPEG bytes rather than a path
// that the model cannot dereference.
type InspectImage struct {
	WS *Workspace
}

func (t *InspectImage) Name() string { return InspectImageToolName }
func (t *InspectImage) Description() string {
	return "Inspect a local PNG or JPEG with the active vision model. The tool reads the image from the authorized workspace and queues validated pixels for the next model boundary; use read_file only for UTF-8 text."
}
func (t *InspectImage) Kind() Kind                       { return KindReadOnly }
func (t *InspectImage) ParallelSafe(map[string]any) bool { return false }
func (t *InspectImage) SubtaskSafe() bool                { return true }

func (t *InspectImage) Schema() map[string]any {
	return obj(map[string]any{
		"file_path": str("Absolute path to a local PNG, JPG, or JPEG file."),
		"detail": map[string]any{
			"type":        "string",
			"enum":        []string{"auto", "low", "high", "original"},
			"description": "Optional provider image detail hint. Defaults to auto.",
		},
	}, "file_path")
}

func (t *InspectImage) ReadIdentity(args map[string]any) (ReadIdentity, error) {
	path, err := t.resolvePath(pathArg(args))
	if err != nil {
		return ReadIdentity{}, err
	}
	detail, err := modelImageDetail(strArg(args, "detail"))
	if err != nil {
		return ReadIdentity{}, err
	}
	file, info, err := openVerifiedModelImage(t.WS, path)
	if err != nil {
		return ReadIdentity{}, err
	}
	_ = file.Close()
	return ReadIdentity{
		Key:  path + "\x00detail=" + detail,
		Path: t.WS.ModelPath(path),
		Version: strconv.FormatInt(info.ModTime().UnixNano(), 10) +
			"\x00" + strconv.FormatInt(info.Size(), 10),
	}, nil
}

func (t *InspectImage) Execute(ctx context.Context, args map[string]any) Result {
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	path, err := t.resolvePath(pathArg(args))
	if err != nil {
		return errResult(err)
	}
	detail, err := modelImageDetail(strArg(args, "detail"))
	if err != nil {
		return errResult(err)
	}
	part, normalized, err := modelImageContentPart(t.WS, path, detail)
	if err != nil {
		return errResultf("inspect image %s: %v", t.WS.ModelPath(path), err)
	}
	if err := ctx.Err(); err != nil {
		return errResult(err)
	}
	t.WS.NoteRead(path)
	content := fmt.Sprintf(
		"MODEL_VISUAL_EVIDENCE_QUEUED: local image pixels were validated and queued for model inspection: %s (%dx%d, %d-byte normalized payload).",
		t.WS.ModelPath(path), normalized.Width, normalized.Height, normalized.DataBytes,
	)
	return Result{
		LLMContent:   content,
		ContentParts: []provider.MessageContentPart{part},
		Display:      "inspected image " + normalized.Name,
	}
}

func (t *InspectImage) resolvePath(raw string) (string, error) {
	if t == nil || t.WS == nil {
		return "", fmt.Errorf("workspace is not configured")
	}
	// Reuse the file reader's canonical, case-tolerant workspace resolution;
	// this does not relax ReadFile's text-only execution boundary.
	return (&ReadFile{WS: t.WS}).resolvePath(raw)
}

func modelImageContentPart(ws *Workspace, path, detail string) (provider.MessageContentPart, normalizedModelImage, error) {
	return modelImageContentPartWithDecodeLimit(ws, path, detail, modelImageMaxDecodedBytes)
}

func modelImageContentPartWithDecodeLimit(ws *Workspace, path, detail string, maxDecodedBytes int) (provider.MessageContentPart, normalizedModelImage, error) {
	normalized, err := normalizeModelImageWithDecodeLimit(ws, path, detail, maxDecodedBytes)
	if err != nil {
		return provider.MessageContentPart{}, normalizedModelImage{}, err
	}
	return provider.MessageContentPart{
		Type:     "image_url",
		ImageURL: &provider.MessageImageURL{URL: normalized.DataURL, Detail: detail},
	}, normalized, nil
}

// normalizeModelImage keeps screenshots and text-heavy references materially
// more legible than the 512 px chat-upload thumbnail path while retaining a
// strict request-size bound. Every accepted file is decoded and encoded again
// from pixels so EXIF, trailing bytes, and other non-visual payloads never
// cross the provider boundary. Oversized inputs are resized and JPEG-compressed.
func normalizeModelImage(ws *Workspace, path, detail string) (normalizedModelImage, error) {
	return normalizeModelImageWithDecodeLimit(ws, path, detail, modelImageMaxDecodedBytes)
}

func normalizeModelImageWithDecodeLimit(ws *Workspace, path, detail string, maxDecodedBytes int) (normalizedModelImage, error) {
	bounds := modelImageBoundsForDetail(detail)
	file, info, err := openVerifiedModelImage(ws, path)
	if err != nil {
		return normalizedModelImage{}, err
	}
	defer file.Close()
	if info.Size() <= 0 || info.Size() > modelImageMaxInputBytes {
		return normalizedModelImage{}, fmt.Errorf(
			"image size %d is outside the 1-%d byte input bound",
			info.Size(), modelImageMaxInputBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, modelImageMaxInputBytes+1))
	if err != nil || int64(len(data)) != info.Size() || len(data) > modelImageMaxInputBytes {
		if err == nil {
			err = fmt.Errorf("image changed or exceeded its verified size while being read")
		}
		return normalizedModelImage{}, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return normalizedModelImage{}, fmt.Errorf("unsupported image: %w", err)
	}
	mediaType := ""
	switch format {
	case "png":
		mediaType = "image/png"
	case "jpeg":
		mediaType = "image/jpeg"
	default:
		return normalizedModelImage{}, fmt.Errorf(
			"unsupported image format %q; only PNG and JPEG are accepted", format,
		)
	}
	pixels := uint64(config.Width) * uint64(config.Height)
	// Budget eight bytes per pixel rather than assuming RGBA8. PNG can decode
	// through 16-bit channels and JPEG decoders retain coefficient/state memory
	// in addition to the final image buffer.
	if maxDecodedBytes <= 0 || pixels > uint64(maxDecodedBytes/8) {
		return normalizedModelImage{}, fmt.Errorf(
			"image dimensions %dx%d exceed the %d-byte decoded-pixel bound",
			config.Width, config.Height, maxDecodedBytes,
		)
	}
	shortSide, longSide := config.Width, config.Height
	if shortSide > longSide {
		shortSide, longSide = longSide, shortSide
	}
	if uint64(longSide) > uint64(shortSide)*modelImageMaxAspectRatio {
		return normalizedModelImage{}, fmt.Errorf(
			"image dimensions %dx%d exceed the %d:1 aspect-ratio bound",
			config.Width, config.Height, modelImageMaxAspectRatio,
		)
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return normalizedModelImage{}, fmt.Errorf("decode complete image: %w", err)
	}
	if decodedFormat != format {
		return normalizedModelImage{}, fmt.Errorf(
			"decoded image format %q does not match header format %q", decodedFormat, format,
		)
	}
	if format == "png" && config.Width <= bounds.MaxDimension && config.Height <= bounds.MaxDimension {
		var canonical bytes.Buffer
		if err := png.Encode(&canonical, decoded); err != nil {
			return normalizedModelImage{}, fmt.Errorf("encode canonical PNG: %w", err)
		}
		if canonical.Len() <= bounds.MaxDataBytes {
			return normalizedModelImage{
				Name: filepath.Base(path), DataURL: modelImageDataURL(mediaType, canonical.Bytes()),
				Width: config.Width, Height: config.Height, DataBytes: canonical.Len(),
			}, nil
		}
	}

	width, height := fitModelImage(config.Width, config.Height, bounds.MaxDimension)
	for attempt := 0; attempt < 7; attempt++ {
		resized := resizeModelImageOnWhite(decoded, width, height)
		for _, quality := range bounds.JPEGQuality {
			var output bytes.Buffer
			if err := jpeg.Encode(&output, resized, &jpeg.Options{Quality: quality}); err != nil {
				return normalizedModelImage{}, fmt.Errorf("encode image: %w", err)
			}
			if output.Len() <= bounds.MaxDataBytes {
				return normalizedModelImage{
					Name: filepath.Base(path), DataURL: modelImageDataURL("image/jpeg", output.Bytes()),
					Width: width, Height: height, DataBytes: output.Len(),
				}, nil
			}
		}
		if width == 1 && height == 1 {
			break
		}
		width = modelImageMaxInt(1, width*4/5)
		height = modelImageMaxInt(1, height*4/5)
	}
	return normalizedModelImage{}, fmt.Errorf(
		"could not fit image within the %d-byte model evidence bound",
		bounds.MaxDataBytes,
	)
}

// openVerifiedModelImage opens the already-authorized path through os.Root and
// returns one stable file descriptor. Component lstat checks reject declared
// symlinks, while os.Root keeps a concurrent path swap confined to the same
// authorized namespace. The final SameFile check binds validation to the fd
// that normalizeModelImage actually reads.
func openVerifiedModelImage(ws *Workspace, path string) (*os.File, os.FileInfo, error) {
	if ws == nil {
		return nil, nil, fmt.Errorf("workspace is not configured")
	}
	rootPath, _, ok := ws.narrowestNamespace(filepath.Clean(path), true)
	if !ok {
		return nil, nil, fmt.Errorf("image path is outside the authorized workspace roots")
	}
	rel, err := filepath.Rel(rootPath, filepath.Clean(path))
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil, fmt.Errorf("image path is outside the authorized workspace root")
	}
	rootBefore, err := os.Lstat(rootPath)
	if err != nil || rootBefore.Mode()&os.ModeSymlink != 0 || !rootBefore.IsDir() {
		return nil, nil, fmt.Errorf("image root is not a verified directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	rootAfter, err := root.Stat(".")
	if err != nil || !os.SameFile(rootBefore, rootAfter) {
		return nil, nil, fmt.Errorf("image root changed while being opened")
	}
	current := ""
	components := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	var before os.FileInfo
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, nil, fmt.Errorf("invalid image path component %q", component)
		}
		current = filepath.Join(current, component)
		before, err = root.Lstat(current)
		if err != nil {
			return nil, nil, err
		}
		if before.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("image path component %q is a symlink", component)
		}
		if index < len(components)-1 && !before.IsDir() {
			return nil, nil, fmt.Errorf("image path component %q is not a directory", component)
		}
	}
	if before == nil || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("image path is not a regular file")
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		file.Close()
		return nil, nil, fmt.Errorf("image file changed while being opened")
	}
	return file, after, nil
}

func modelImageBoundsForDetail(detail string) modelImageBounds {
	switch detail {
	case "low":
		return modelImageBounds{
			MaxDimension: 768, MaxDataBytes: 256 << 10,
			JPEGQuality: []int{82, 72, 60},
		}
	case "high":
		return modelImageBounds{
			MaxDimension: 2048, MaxDataBytes: 1 << 20,
			JPEGQuality: []int{92, 86, 80, 72, 64},
		}
	case "original":
		return modelImageBounds{
			MaxDimension: 4096, MaxDataBytes: 1 << 20,
			JPEGQuality: []int{94, 88, 82, 74, 66},
		}
	default:
		return modelImageBounds{
			MaxDimension: 1536, MaxDataBytes: 768 << 10,
			JPEGQuality: []int{90, 82, 74, 64},
		}
	}
}

func fitModelImage(width, height, maxDimension int) (int, int) {
	if width <= maxDimension && height <= maxDimension {
		return width, height
	}
	if width >= height {
		return maxDimension, modelImageMaxInt(1, height*maxDimension/width)
	}
	return modelImageMaxInt(1, width*maxDimension/height), maxDimension
}

func modelImageMaxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func resizeModelImageOnWhite(source image.Image, width, height int) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := source.Bounds()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + x*bounds.Dx()/width
			sy := bounds.Min.Y + y*bounds.Dy()/height
			r, g, b, a := source.At(sx, sy).RGBA()
			target.SetRGBA(x, y, color.RGBA{
				R: uint8((r + 0xffff - a) >> 8),
				G: uint8((g + 0xffff - a) >> 8),
				B: uint8((b + 0xffff - a) >> 8),
				A: 0xff,
			})
		}
	}
	return target
}

func modelImageDataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func modelImageDetail(raw string) (string, error) {
	detail := strings.ToLower(strings.TrimSpace(raw))
	if detail == "" {
		return "auto", nil
	}
	switch detail {
	case "auto", "low", "high", "original":
		return detail, nil
	default:
		return "", fmt.Errorf("unsupported image detail %q", raw)
	}
}
