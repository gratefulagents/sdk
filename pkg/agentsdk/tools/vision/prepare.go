package vision

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	// MaxImageLongEdge is the longest side, in pixels, of an image attached to
	// a model conversation.
	MaxImageLongEdge = 2000
	// MaxImageBytes is the largest encoded image attached to a model
	// conversation. Base64 inflates it to ~5MB, the per-image provider limit.
	MaxImageBytes = 3_750_000
	// maxDecodePixels bounds decode memory for hostile or huge images.
	maxDecodePixels  = 50_000_000
	minImageLongEdge = 64
)

var jpegQualities = []int{85, 70, 55, 40}

// PreparedImage is an image ready to attach to a model conversation.
type PreparedImage struct {
	Data           []byte
	MediaType      string
	Width          int
	Height         int
	OriginalWidth  int
	OriginalHeight int
}

// Resized reports whether the attached image is smaller than the source.
func (p PreparedImage) Resized() bool {
	return p.Width != p.OriginalWidth || p.Height != p.OriginalHeight
}

// IsImagePath reports whether path has an image extension that PrepareImage
// can decode.
func IsImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// PrepareImage decodes a PNG, JPEG, GIF, or WebP image and returns a version
// whose long edge is at most MaxImageLongEdge and whose encoded size is at
// most MaxImageBytes. Images already within both limits are returned
// unchanged. Oversized images are downscaled and re-encoded as PNG when that
// fits (lossless sources only), otherwise as JPEG with decreasing quality,
// shrinking further until the size limit is met.
func PrepareImage(data []byte) (PreparedImage, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return PreparedImage{}, fmt.Errorf("decoding image: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return PreparedImage{}, fmt.Errorf("image has invalid dimensions %dx%d", cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return PreparedImage{}, fmt.Errorf("image dimensions %dx%d exceed %d pixels", cfg.Width, cfg.Height, maxDecodePixels)
	}
	result := PreparedImage{
		MediaType:      "image/" + format,
		Width:          cfg.Width,
		Height:         cfg.Height,
		OriginalWidth:  cfg.Width,
		OriginalHeight: cfg.Height,
	}
	if max(cfg.Width, cfg.Height) <= MaxImageLongEdge && len(data) <= MaxImageBytes {
		result.Data = data
		return result, nil
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return PreparedImage{}, fmt.Errorf("decoding image: %w", err)
	}
	w, h := fitLongEdge(cfg.Width, cfg.Height, MaxImageLongEdge)
	img := resize(src, w, h)

	if format == "png" || format == "gif" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return PreparedImage{}, fmt.Errorf("encoding png: %w", err)
		}
		if buf.Len() <= MaxImageBytes {
			result.Data, result.MediaType, result.Width, result.Height = buf.Bytes(), "image/png", w, h
			return result, nil
		}
	}

	flat := flattenOnWhite(img)
	for {
		for _, quality := range jpegQualities {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: quality}); err != nil {
				return PreparedImage{}, fmt.Errorf("encoding jpeg: %w", err)
			}
			if buf.Len() <= MaxImageBytes {
				result.Data, result.MediaType, result.Width, result.Height = buf.Bytes(), "image/jpeg", w, h
				return result, nil
			}
		}
		nw, nh := w*3/4, h*3/4
		if max(nw, nh) < minImageLongEdge || nw < 1 || nh < 1 {
			return PreparedImage{}, fmt.Errorf("cannot shrink image below %d bytes", MaxImageBytes)
		}
		w, h = nw, nh
		flat = resize(flat, w, h)
	}
}

func fitLongEdge(w, h, limit int) (int, int) {
	long := max(w, h)
	if long <= limit {
		return w, h
	}
	nw := max(1, int(int64(w)*int64(limit)/int64(long)))
	nh := max(1, int(int64(h)*int64(limit)/int64(long)))
	return nw, nh
}

func resize(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	if b.Dx() == w && b.Dy() == h {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

// flattenOnWhite composites transparent pixels over white because JPEG has
// no alpha channel and would otherwise render them black.
func flattenOnWhite(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}
