package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/url"
	"path/filepath"
	"strings"
)

const (
	presentationMarkdownMaxAssetBytes        = 2 << 20
	presentationMarkdownMaxTotalAssetBytes   = 8 << 20
	presentationMarkdownMaxAssets            = 8
	presentationMarkdownMaxImagePixels       = 16_000_000
	presentationMarkdownMaxAssetRequestBytes = 12 << 20
)

type presentonMarkdownAssetMetadata struct {
	SHA256      string `json:"sha256"`
	MIME        string `json:"mime"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ByteLength  int    `json:"byte_length"`
	Placeholder bool   `json:"placeholder"`
}

type presentonMarkdownAsset struct {
	presentonMarkdownAssetMetadata
	Base64 string `json:"base64"`
}

type presentonMarkdownImagePlacement struct {
	AssetID     string `json:"asset_id"`
	Slot        string `json:"slot"`
	Alt         string `json:"alt"`
	Placeholder bool   `json:"placeholder"`
}

func validateMarkdownImageSource(source string) error {
	if source == "placeholder:photo" {
		return nil
	}
	parsed, err := url.Parse(source)
	if err != nil || source == "" || strings.TrimSpace(source) != source || len(source) > presentationMarkdownMaxURLBytes || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(source, "\\") {
		return fmt.Errorf("images require a local workspace path or explicit placeholder:photo; URL, data and file schemes are not fetched")
	}
	return nil
}

// Unlike general file reads, image provenance must not silently remap an
// invented absolute path to a matching workspace basename. Retain the declared
// path components so the strict descriptor opener can reject ancestor symlinks.
func strictMarkdownImagePath(ws *Workspace, source string) (string, error) {
	if ws == nil {
		return "", fmt.Errorf("workspace is required")
	}
	if err := validateMarkdownImageSource(source); err != nil || source == "placeholder:photo" {
		return "", fmt.Errorf("expected a local supplied image path")
	}
	path, err := url.PathUnescape(source)
	if err != nil || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("invalid encoded image path")
	}
	for _, component := range strings.Split(path, "/") {
		if component == ".." {
			return "", fmt.Errorf("image paths cannot contain parent traversal")
		}
	}
	for _, mount := range []struct{ name, root string }{{"/session", ws.SessionRoot}, {"/workspace", ws.Root}, {"/workdir", ws.Root}} {
		if relative, ok := mountedRelative(path, mount.name); ok {
			if mount.root == "" {
				return "", fmt.Errorf("image mount is not configured")
			}
			return filepath.Join(mount.root, filepath.FromSlash(relative)), nil
		}
	}
	if !filepath.IsAbs(path) {
		return filepath.Join(ws.Root, filepath.FromSlash(path)), nil
	}
	root, _, ok := ws.narrowestNamespace(normalizePathSpellingKeepParents(path), false)
	if !ok || !containsRoot(root, filepath.Clean(path)) {
		return "", fmt.Errorf("image path is outside the authorized workspace and session roots")
	}
	return filepath.Clean(path), nil
}

func readMarkdownImageAsset(ctx context.Context, ws *Workspace, source string) ([]byte, presentonMarkdownAssetMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, presentonMarkdownAssetMetadata{}, err
	}
	path, err := strictMarkdownImagePath(ws, source)
	if err != nil {
		return nil, presentonMarkdownAssetMetadata{}, err
	}
	file, before, err := openVerifiedModelImage(ws, path)
	if err != nil {
		return nil, presentonMarkdownAssetMetadata{}, err
	}
	defer file.Close()
	if before.Size() < 1 || before.Size() > presentationMarkdownMaxAssetBytes {
		return nil, presentonMarkdownAssetMetadata{}, fmt.Errorf("supplied image exceeds the 2 MiB encoded-file limit or is empty")
	}
	data, err := io.ReadAll(io.LimitReader(file, presentationMarkdownMaxAssetBytes+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || len(data) > presentationMarkdownMaxAssetBytes || int64(len(data)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, presentonMarkdownAssetMetadata{}, fmt.Errorf("supplied image changed during its bounded read")
	}
	metadata, err := validateMarkdownImageBytes(data, false)
	if err != nil {
		return nil, presentonMarkdownAssetMetadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, presentonMarkdownAssetMetadata{}, err
	}
	ws.NoteRead(path)
	return data, metadata, nil
}

func validateMarkdownImageBytes(data []byte, placeholder bool) (presentonMarkdownAssetMetadata, error) {
	if len(data) == 0 || len(data) > presentationMarkdownMaxAssetBytes {
		return presentonMarkdownAssetMetadata{}, fmt.Errorf("image bytes must fit the 2 MiB limit")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width <= 0 || config.Height <= 0 {
		return presentonMarkdownAssetMetadata{}, fmt.Errorf("supplied image must be a valid PNG or JPEG")
	}
	if uint64(config.Width)*uint64(config.Height) > presentationMarkdownMaxImagePixels {
		return presentonMarkdownAssetMetadata{}, fmt.Errorf("supplied image exceeds the 16 MP decoded-pixel limit")
	}
	decoded, actualFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || actualFormat != format || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return presentonMarkdownAssetMetadata{}, fmt.Errorf("supplied image failed full PNG/JPEG decoding")
	}
	hash := sha256.Sum256(data)
	mime := "image/" + format
	return presentonMarkdownAssetMetadata{SHA256: hex.EncodeToString(hash[:]), MIME: mime, Width: config.Width, Height: config.Height, ByteLength: len(data), Placeholder: placeholder}, nil
}

func markdownPlaceholderImage() ([]byte, error) {
	canvas := image.NewNRGBA(image.Rect(0, 0, 800, 600))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.NRGBA{R: 220, G: 222, B: 224, A: 255}), image.Point{}, draw.Src)
	var buffer bytes.Buffer
	err := png.Encode(&buffer, canvas)
	return buffer.Bytes(), err
}

// Images are read once per declared path and deduplicated by exact supplied
// bytes plus placeholder status. Only after the whole bounded set succeeds does the IR get
// asset IDs, so failure/cancellation cannot leave a partially resolved draft.
func resolveMarkdownAssets(ctx context.Context, ws *Workspace, deck *markdownPresentation) (map[string]presentonMarkdownAsset, error) {
	assets := map[string]presentonMarkdownAsset{}
	updates := make([][]markdownImage, len(deck.Slides))
	bySource := map[string]string{}
	total := 0
	for slideIndex, slide := range deck.Slides {
		updates[slideIndex] = append([]markdownImage(nil), slide.Images...)
		for imageIndex, authored := range slide.Images {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			id, seen := bySource[authored.Source]
			if !seen {
				var data []byte
				var metadata presentonMarkdownAssetMetadata
				var err error
				if authored.Source == "placeholder:photo" {
					data, err = markdownPlaceholderImage()
					if err == nil {
						metadata, err = validateMarkdownImageBytes(data, true)
					}
				} else {
					data, metadata, err = readMarkdownImageAsset(ctx, ws, authored.Source)
				}
				if err != nil {
					return nil, fmt.Errorf("markdown line %d: %w", authored.Line, err)
				}
				id = "img-" + metadata.SHA256
				if metadata.Placeholder {
					id = "placeholder-" + metadata.SHA256
				}
				if _, exists := assets[id]; !exists {
					if len(assets) >= presentationMarkdownMaxAssets || total+len(data) > presentationMarkdownMaxTotalAssetBytes {
						return nil, fmt.Errorf("Markdown images exceed 8 unique assets or 8 MiB total raw bytes; none were rendered")
					}
					total += len(data)
					assets[id] = presentonMarkdownAsset{presentonMarkdownAssetMetadata: metadata, Base64: base64.StdEncoding.EncodeToString(data)}
				}
				bySource[authored.Source] = id
			}
			updates[slideIndex][imageIndex].AssetID = id
			updates[slideIndex][imageIndex].Placeholder = assets[id].Placeholder
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for index := range deck.Slides {
		deck.Slides[index].Images = updates[index]
	}
	return assets, nil
}

func markdownAssetMetadata(assets map[string]presentonMarkdownAsset) map[string]presentonMarkdownAssetMetadata {
	metadata := map[string]presentonMarkdownAssetMetadata{}
	for id, asset := range assets {
		metadata[id] = asset.presentonMarkdownAssetMetadata
	}
	return metadata
}

func (compiled *presentationMarkdownImport) sidecars() []string {
	names := append([]string(nil), presentationMarkdownSidecars...)
	if len(compiled.Assets) != 0 {
		names = append(names, "image-assets.json")
	}
	return names
}

func encodeMarkdownAssetSidecar(deck *markdownPresentation, assets map[string]presentonMarkdownAsset) ([]byte, error) {
	placements := []map[string]any{}
	for _, slide := range deck.Slides {
		if len(slide.Images) != 0 {
			placements = append(placements, map[string]any{"slide": slide.Number, "images": slide.Images})
		}
	}
	data, err := json.MarshalIndent(map[string]any{"version": 1, "assets": markdownAssetMetadata(assets), "placements": placements}, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data)+1 > presentationMarkdownMaxImagePlanBytes {
		return nil, fmt.Errorf("image-asset provenance exceeds 256 KiB; no assets were rendered")
	}
	return append(data, '\n'), nil
}
