package vision

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"testing"
)

func TestShrinkLeavesSmallImageUntouched(t *testing.T) {
	mimeType, data, err := Shrink("image/png", pngPixel, DefaultMaxImageBytes)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := "image/png", mimeType; e != g {
		t.Errorf("mimeType: expected %q, got %q", e, g)
	}

	if !bytes.Equal(pngPixel, data) {
		t.Error("data: expected the original bytes to be returned untouched")
	}
}

func TestShrinkReEncodesOversizedImage(t *testing.T) {
	// A noisy PNG is incompressible, which is exactly what makes a screenshot
	// heavy: the size comes from the lossless format, not from the detail.
	source := noisyPNG(t, 800, 600)

	const maxBytes = 60_000

	if int64(len(source)) <= maxBytes {
		t.Fatalf("test image is %d bytes, expected more than %d", len(source), maxBytes)
	}

	mimeType, data, err := Shrink("image/png", source, maxBytes)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := "image/jpeg", mimeType; e != g {
		t.Errorf("mimeType: expected %q, got %q", e, g)
	}

	if int64(len(data)) > maxBytes {
		t.Errorf("len(data): expected at most %d bytes, got %d", maxBytes, len(data))
	}

	// The result must remain a decodable image, and keep the aspect ratio of
	// the source.
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("could not decode the shrunk image: %+v", err)
	}

	if e, g := "jpeg", format; e != g {
		t.Errorf("format: expected %q, got %q", e, g)
	}

	if ratio := float64(config.Width) / float64(config.Height); ratio < 1.29 || ratio > 1.37 {
		t.Errorf("aspect ratio: expected ~%.2f, got %.2f (%dx%d)", 800.0/600.0, ratio, config.Width, config.Height)
	}
}

func TestShrinkCapsDimension(t *testing.T) {
	source := noisyPNG(t, 3000, 200)

	_, data, err := Shrink("image/png", source, int64(len(source))-1)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("could not decode the shrunk image: %+v", err)
	}

	if config.Width > ShrinkMaxDimension {
		t.Errorf("config.Width: expected at most %d, got %d", ShrinkMaxDimension, config.Width)
	}
}

func TestShrinkRefusesUndecodableImage(t *testing.T) {
	if _, _, err := Shrink("image/svg+xml", []byte("<svg></svg>"), 4); err == nil {
		t.Error("expected an error for an image no Go decoder handles")
	}
}

func TestShrinkRefusesUnreachableLimit(t *testing.T) {
	// Below a legible size the loop gives up rather than handing the model a
	// thumbnail nothing can be read from.
	if _, _, err := Shrink("image/png", noisyPNG(t, 800, 600), 64); err == nil {
		t.Error("expected an error for a limit no legible image can fit in")
	}
}

// noisyPNG builds an incompressible PNG of the given dimensions.
func noisyPNG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))

	random := rand.New(rand.NewSource(1))

	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{
				R: uint8(random.Intn(256)),
				G: uint8(random.Intn(256)),
				B: uint8(random.Intn(256)),
				A: 255,
			})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("could not encode the test image: %+v", err)
	}

	return buf.Bytes()
}
