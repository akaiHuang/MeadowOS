// Package attachments contains the small, shared image normalizer used by the
// terminal and Desktop clients before a prompt is sent to a vision model.
package attachments

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxWidth       = 512
	MaxDataBytes   = 100 * 1024
	MaxInputBytes  = 20 * 1024 * 1024
	maxEncodeTries = 6
)

// Normalized is a data URL ready for provider.MessageContentPart.ImageURL.
type Normalized struct {
	Name      string
	DataURL   string
	Width     int
	Height    int
	DataBytes int
}

// NormalizeFile reads a local image and bounds its width and encoded payload.
// Small images are preserved byte-for-byte so screenshots and existing clients
// do not unexpectedly change format; larger images are resized and JPEG encoded.
func NormalizeFile(path string) (Normalized, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Normalized{}, fmt.Errorf("image path is empty")
	}
	st, err := os.Stat(path)
	if err != nil {
		return Normalized{}, err
	}
	if st.IsDir() {
		return Normalized{}, fmt.Errorf("image path is a directory")
	}
	if st.Size() > MaxInputBytes {
		return Normalized{}, fmt.Errorf("image is too large (limit %d MB)", MaxInputBytes/(1024*1024))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Normalized{}, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return Normalized{}, fmt.Errorf("unsupported image: %w", err)
	}
	name := filepath.Base(path)
	if len(b) <= MaxDataBytes && cfg.Width <= MaxWidth {
		mime := mimeForFormat(format, filepath.Ext(path))
		return Normalized{Name: name, DataURL: dataURL(mime, b), Width: cfg.Width, Height: cfg.Height, DataBytes: len(b)}, nil
	}

	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return Normalized{}, fmt.Errorf("decode image: %w", err)
	}
	width, height := fit(cfg.Width, cfg.Height, MaxWidth)
	for attempt := 0; attempt < maxEncodeTries; attempt++ {
		resized := resizeNearest(img, width, height)
		for _, quality := range []int{82, 70, 55, 42} {
			var out bytes.Buffer
			if err := jpeg.Encode(&out, resized, &jpeg.Options{Quality: quality}); err != nil {
				return Normalized{}, fmt.Errorf("encode image: %w", err)
			}
			if out.Len() <= MaxDataBytes || (attempt == maxEncodeTries-1 && quality == 42) {
				return Normalized{Name: name, DataURL: dataURL("image/jpeg", out.Bytes()), Width: width, Height: height, DataBytes: out.Len()}, nil
			}
		}
		width = maxInt(1, width*4/5)
		height = maxInt(1, height*4/5)
	}
	return Normalized{}, fmt.Errorf("could not normalize image")
}

func fit(width, height, maxWidth int) (int, int) {
	if width <= maxWidth {
		return width, height
	}
	return maxWidth, maxInt(1, height*maxWidth/width)
}

func resizeNearest(src image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	sb := src.Bounds()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			sx := sb.Min.X + x*sb.Dx()/width
			sy := sb.Min.Y + y*sb.Dy()/height
			dst.Set(x, y, color.RGBAModel.Convert(src.At(sx, sy)))
		}
	}
	return dst
}

func dataURL(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func mimeForFormat(format, ext string) string {
	switch strings.ToLower(format) {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "jpeg", "jpg":
		return "image/jpeg"
	default:
		switch strings.ToLower(ext) {
		case ".png":
			return "image/png"
		case ".gif":
			return "image/gif"
		default:
			return "image/jpeg"
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Keep the PNG decoder linked for installations that use image/png files via
// DecodeConfig before a platform-specific decoder is otherwise referenced.
var _ = png.DecodeConfig
