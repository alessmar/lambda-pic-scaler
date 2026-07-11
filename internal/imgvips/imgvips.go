// Package imgvips resizes images with libvips (via cgo), using shrink-on-load thumbnailing.
package imgvips

import (
	"fmt"

	"github.com/davidbyttow/govips/v2/vips"

	"github.com/alessmar/lambda-pic-scaler/scaler"
)

// Startup and Shutdown wrap the libvips lifecycle: call once per process.
func Startup()  { vips.LoggingSettings(nil, vips.LogLevelWarning); vips.Startup(nil) }
func Shutdown() { vips.Shutdown() }

// Resize fits src inside spec (enlarging it if it is smaller), never producing a side above scaler.MaxDim, keeps the format, applies EXIF orientation
// and strips metadata. Only JPEG, PNG and WebP are accepted: anything else is scaler.ErrUnsupported.
func Resize(src []byte, spec scaler.Spec) ([]byte, string, error) {
	switch vips.DetermineImageType(src) {
	case vips.ImageTypeJPEG, vips.ImageTypePNG, vips.ImageTypeWEBP:
	default:
		return nil, "", scaler.ErrUnsupported
	}
	w, h := spec.W, spec.H
	// A missing side is bounded by MaxDim, not left open: the derived side of a very tall or wide image
	// could otherwise end up far above MaxDim (and exhaust the Lambda memory).
	if w == 0 {
		w = scaler.MaxDim
	}
	if h == 0 {
		h = scaler.MaxDim
	}
	img, err := vips.NewThumbnailWithSizeFromBuffer(src, w, h, vips.InterestingNone, vips.SizeBoth)
	if err != nil {
		return nil, "", fmt.Errorf("thumbnail: %w", err)
	}
	defer img.Close()

	switch img.Format() {
	case vips.ImageTypeJPEG:
		b, _, err := img.ExportJpeg(&vips.JpegExportParams{Quality: 85, StripMetadata: true, Interlace: true})
		return b, "image/jpeg", err
	case vips.ImageTypePNG:
		b, _, err := img.ExportPng(&vips.PngExportParams{Compression: 6, StripMetadata: true})
		return b, "image/png", err
	default:
		b, _, err := img.ExportWebp(&vips.WebpExportParams{Quality: 85, StripMetadata: true})
		return b, "image/webp", err
	}
}
