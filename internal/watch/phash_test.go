package watch

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gradient is a picture with something in it: a flat image hashes to nothing useful, and a
// sawtooth one is not a photograph either. This is smooth, with a darker block for shape.
func gradient(w, h int, seed int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x*255/w + y*255/h + seed*40) / 2 % 256)
			if x > w/3 && x < 2*w/3 && y > h/4 && y < h/2 {
				v /= 3
			}
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// noise is the control: two unrelated photographs must not land close.
func noise(w, h int, seed uint64) image.Image {
	rng := rand.New(rand.NewPCG(seed, seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(rng.UintN(256))
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// shrink resamples a picture the way a thumbnail is made.
func shrink(src image.Image, w, h int) image.Image {
	bounds := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out.Set(x, y, src.At(bounds.Min.X+bounds.Dx()*x/w, bounds.Min.Y+bounds.Dy()*y/h))
		}
	}
	return out
}

// The same photograph uploaded again comes back at another size and through another pass
// of JPEG, which is what the threshold has to survive.
func TestHashSurvivesResizeAndCompression(t *testing.T) {
	photo := gradient(320, 240, 1)
	original := Hash(photo)
	smaller := Hash(shrink(photo, 160, 120))
	if d := Distance(original, smaller); d > SamePhoto {
		t.Errorf("the same picture at half the size sat %d bits away", d)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, photo, &jpeg.Options{Quality: 40}); err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if d := Distance(original, Hash(decoded)); d > SamePhoto {
		t.Errorf("the same picture recompressed sat %d bits away", d)
	}
}

func TestHashTellsPicturesApart(t *testing.T) {
	if d := Distance(Hash(noise(64, 64, 1)), Hash(noise(64, 64, 2))); d <= SamePhoto {
		t.Errorf("two unrelated pictures sat %d bits apart, within the threshold", d)
	}
}

func TestHasherReadsAndSkips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		_ = jpeg.Encode(w, gradient(320, 240, 2), nil)
	}))
	defer srv.Close()

	hasher := NewHasher(2)
	got := hasher.Hashes(context.Background(), []string{srv.URL + "/a.jpg", srv.URL + "/broken", srv.URL + "/c.jpg"})
	if len(got) != 1 {
		t.Fatalf("expected one hash from one readable photo within the limit, got %d", len(got))
	}
	if got[0] == 0 {
		t.Error("a readable photo hashed to zero")
	}
}

func TestClosestPhoto(t *testing.T) {
	if got := closestPhoto(nil, []uint64{1}); got != 65 {
		t.Errorf("a listing with no hash answered %d, expected 65", got)
	}
	if got := closestPhoto([]uint64{0b1010, 0b1111}, []uint64{0b1110}); got != 1 {
		t.Errorf("closest = %d, expected 1", got)
	}
}
