package vision

import (
	"bytes"
	"image"
	"image/jpeg"
	"math"

	// Decoders for the formats the vision providers accept (JPEG comes with the
	// import above). WebP is not in the standard library, so it is pulled from
	// golang.org/x/image.
	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"github.com/pkg/errors"
	xdraw "golang.org/x/image/draw"
)

// DefaultMaxSourceBytes bounds the size of an image *before* shrinking: an
// image between MaxImageBytes and this limit is re-encoded to fit (see
// Shrink), a larger one is refused outright. It exists because shrinking
// decodes the pixels into memory — a 300 MiB PNG is not worth the RAM of a
// whole ingestion worker.
const DefaultMaxSourceBytes int64 = 64 << 20 // 64 MiB

// ShrinkQuality is the JPEG quality used when re-encoding an oversized image.
// High enough to keep the small text of a screenshot or a dashboard legible,
// which is precisely what the description has to transcribe.
const ShrinkQuality = 85

// ShrinkMaxDimension caps the longest side of a re-encoded image. The vision
// providers downscale anything larger anyway (Anthropic works at ~1568px), so
// carrying more pixels costs bytes without buying any detail.
const ShrinkMaxDimension = 2048

// shrinkMinDimension is the shortest side worth handing to the model: below
// it the image is unreadable, and shrinking further would be pointless.
const shrinkMinDimension = 64

// shrinkMaxAttempts bounds the re-encoding loop. Each attempt divides the
// pixel count by at least ~1.2, so the loop converges long before the bound.
const shrinkMaxAttempts = 8

// Shrink re-encodes data as JPEG so that it fits within maxBytes, and reports
// the media type of the result. Data already under the limit is returned
// untouched, with its original media type.
//
// It is what keeps a big screenshot — a Grafana dashboard exported as a 12 MiB
// PNG is the typical case — from failing the indexation of the file that
// carries it: those images are large because PNG stores them losslessly, not
// because they hold that much detail.
//
// An image that cannot be decoded (no Go decoder for the format) or that
// cannot be brought under the limit without falling below a legible size is an
// error: the caller decides whether that is fatal.
func Shrink(mimeType string, data []byte, maxBytes int64) (string, []byte, error) {
	if maxBytes <= 0 || int64(len(data)) <= maxBytes {
		return mimeType, data, nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", nil, errors.Wrapf(err, "could not decode image (media type '%s')", mimeType)
	}

	// A JPEG has no alpha channel: composite on white first, otherwise every
	// transparent pixel of a PNG screenshot turns black.
	flattened := flatten(img)

	if resized := capDimension(flattened, ShrinkMaxDimension); resized != nil {
		flattened = resized
	}

	for range shrinkMaxAttempts {
		encoded, err := encodeJPEG(flattened)
		if err != nil {
			return "", nil, errors.WithStack(err)
		}

		if int64(len(encoded)) <= maxBytes {
			return "image/jpeg", encoded, nil
		}

		// JPEG size grows roughly with the pixel count, so the side scales with
		// the square root of the overshoot; the extra margin absorbs the fact
		// that the relation is only approximate.
		ratio := math.Sqrt(float64(maxBytes)/float64(len(encoded))) * 0.9

		resized := scale(flattened, ratio)
		if resized == nil {
			break
		}

		flattened = resized
	}

	return "", nil, errors.Errorf("could not shrink image below %d bytes", maxBytes)
}

// flatten composites img over a white background, producing an opaque image
// suitable for JPEG encoding.
func flatten(img image.Image) *image.RGBA {
	bounds := img.Bounds()

	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))

	xdraw.Draw(dst, dst.Bounds(), image.NewUniform(image.White), image.Point{}, xdraw.Src)
	xdraw.Draw(dst, dst.Bounds(), img, bounds.Min, xdraw.Over)

	return dst
}

// capDimension scales img down so that its longest side is at most maxSide,
// returning nil when it already fits.
func capDimension(img *image.RGBA, maxSide int) *image.RGBA {
	bounds := img.Bounds()

	longest := max(bounds.Dx(), bounds.Dy())
	if longest <= maxSide {
		return nil
	}

	return scale(img, float64(maxSide)/float64(longest))
}

// scale resizes img by ratio, returning nil when the result would drop below a
// legible size. CatmullRom is used rather than a nearest-neighbour resize
// because the point is to keep the *text* of a screenshot readable.
func scale(img *image.RGBA, ratio float64) *image.RGBA {
	if ratio <= 0 || ratio >= 1 {
		return nil
	}

	bounds := img.Bounds()

	width := int(float64(bounds.Dx()) * ratio)
	height := int(float64(bounds.Dy()) * ratio)

	if width < shrinkMinDimension || height < shrinkMinDimension {
		return nil
	}

	dst := image.NewRGBA(image.Rect(0, 0, width, height))

	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, xdraw.Src, nil)

	return dst
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer

	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: ShrinkQuality}); err != nil {
		return nil, errors.WithStack(err)
	}

	return buf.Bytes(), nil
}
