package imgvips

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/alessmar/lambda-pic-scaler/scaler"
)

func TestMain(m *testing.M) {
	Startup()
	defer Shutdown()
	m.Run()
}

func src(t *testing.T, enc func(*bytes.Buffer, image.Image) error) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := range 400 {
		for y := range 200 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 99, 255})
		}
	}
	var b bytes.Buffer
	if err := enc(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestResize(t *testing.T) {
	jpg := src(t, func(b *bytes.Buffer, i image.Image) error { return jpeg.Encode(b, i, nil) })
	pn := src(t, func(b *bytes.Buffer, i image.Image) error { return png.Encode(b, i) })
	for _, c := range []struct {
		name string
		in   []byte
		spec scaler.Spec
		w, h int
		ct   string
	}{
		{"fit box", jpg, scaler.Spec{W: 100, H: 100}, 100, 50, "image/jpeg"},
		{"width only", jpg, scaler.Spec{W: 100}, 100, 50, "image/jpeg"},
		{"height only", pn, scaler.Spec{H: 50}, 100, 50, "image/png"},
		{"enlarges a small image", pn, scaler.Spec{W: 4000}, 4000, 2000, "image/png"},
		{"enlarges into a box", jpg, scaler.Spec{W: 800, H: 800}, 800, 400, "image/jpeg"},
	} {
		out, ct, err := Resize(c.in, c.spec)
		if err != nil || ct != c.ct {
			t.Fatalf("%s: %v %s", c.name, err, ct)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil || cfg.Width != c.w || cfg.Height != c.h {
			t.Errorf("%s: got %dx%d, %v", c.name, cfg.Width, cfg.Height, err)
		}
	}
}

func TestUnsupported(t *testing.T) {
	if _, _, err := Resize([]byte("not an image"), scaler.Spec{W: 10}); !errors.Is(err, scaler.ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

// Only one side given: the derived side must not run past MaxDim either (a tall image
// asked for w=MaxDim would otherwise become 8192 x 81920 pixels).
func TestDerivedSideStaysWithinMaxDim(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 1000))
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	out, _, err := Resize(b.Bytes(), scaler.Spec{W: scaler.MaxDim})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width > scaler.MaxDim || cfg.Height > scaler.MaxDim {
		t.Fatalf("got %dx%d, %v; want both sides <= %d", cfg.Width, cfg.Height, err, scaler.MaxDim)
	}
}
