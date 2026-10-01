package vision

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func solidImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	return img
}

func noiseImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	rng.Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	return img
}

func decodeDims(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode prepared image: %v", err)
	}
	return cfg.Width, cfg.Height, format
}

func TestPrepareImagePassesThroughSmallImage(t *testing.T) {
	data := encodePNG(t, solidImage(40, 20))
	got, err := PrepareImage(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data, data) || got.MediaType != "image/png" || got.Width != 40 || got.Height != 20 || got.Resized() {
		t.Fatalf("PrepareImage() = %+v", got)
	}
}

func TestPrepareImageDetectsFormatFromBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, solidImage(10, 10), nil); err != nil {
		t.Fatal(err)
	}
	got, err := PrepareImage(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaType != "image/jpeg" {
		t.Fatalf("MediaType = %q, want image/jpeg", got.MediaType)
	}
}

func TestPrepareImageDownscalesLongEdgeKeepingPNG(t *testing.T) {
	data := encodePNG(t, solidImage(3000, 1500))
	got, err := PrepareImage(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 2000 || got.Height != 1000 || got.OriginalWidth != 3000 || got.OriginalHeight != 1500 || !got.Resized() {
		t.Fatalf("PrepareImage() dims = %dx%d (from %dx%d)", got.Width, got.Height, got.OriginalWidth, got.OriginalHeight)
	}
	w, h, format := decodeDims(t, got.Data)
	if w != 2000 || h != 1000 || format != "png" || got.MediaType != "image/png" {
		t.Fatalf("encoded = %dx%d %s (%s)", w, h, format, got.MediaType)
	}
}

func TestPrepareImageFallsBackToJPEGUnderByteLimit(t *testing.T) {
	data := encodePNG(t, noiseImage(1900, 1900))
	if len(data) <= MaxImageBytes {
		t.Fatalf("fixture too small: %d bytes", len(data))
	}
	got, err := PrepareImage(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Data) > MaxImageBytes {
		t.Fatalf("prepared size = %d, want <= %d", len(got.Data), MaxImageBytes)
	}
	w, h, format := decodeDims(t, got.Data)
	if format != "jpeg" || got.MediaType != "image/jpeg" || w != got.Width || h != got.Height || w > MaxImageLongEdge {
		t.Fatalf("encoded = %dx%d %s, result = %+v", w, h, format, got.MediaType)
	}
}

func TestPrepareImageDecodesWebP(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	got, err := PrepareImage(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaType != "image/webp" || got.Width != 1 || got.Height != 1 {
		t.Fatalf("PrepareImage() = %+v", got)
	}
}

func TestPrepareImageRejectsNonImage(t *testing.T) {
	if _, err := PrepareImage([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>")); err == nil {
		t.Fatal("PrepareImage() error = nil, want decode failure")
	}
}

func TestIsImagePath(t *testing.T) {
	for path, want := range map[string]bool{
		"a.png": true, "b.JPG": true, "c.jpeg": true, "d.gif": true, "e.webp": true,
		"f.svg": false, "g.go": false, "png": false,
	} {
		if got := IsImagePath(path); got != want {
			t.Fatalf("IsImagePath(%q) = %v, want %v", path, got, want)
		}
	}
}
