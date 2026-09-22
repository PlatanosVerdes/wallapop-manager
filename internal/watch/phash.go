package watch

import (
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math/bits"
	"net/http"
	"strings"
	"time"
)

// A listing reposted from another town is the same photographs uploaded again, so the
// picture is the thing that identifies it, not the words. dHash reduces one to 64 bits:
// the thumbnail is shrunk to a 9x8 grey grid and each bit says whether a cell is brighter
// than the one on its right. Re-compression moves a bit or two and nothing else, so two
// hashes within a few bits are the same photograph.
const (
	hashWidth  = 9
	hashHeight = 8
	// MaxPhotoBytes is the ceiling on a thumbnail download. The small size is ~30 KB.
	MaxPhotoBytes = 2 << 20
)

// Distance counts the bits two hashes disagree on.
func Distance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// Hash reduces an image to its dHash. It walks the source once per output cell, which for
// a 320 px thumbnail is the whole image and a few hundred microseconds on the Pi.
func Hash(img image.Image) uint64 {
	bounds := img.Bounds()
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return 0
	}

	var grid [hashHeight][hashWidth]float64
	for row := 0; row < hashHeight; row++ {
		for col := 0; col < hashWidth; col++ {
			x0 := bounds.Min.X + bounds.Dx()*col/hashWidth
			x1 := bounds.Min.X + bounds.Dx()*(col+1)/hashWidth
			y0 := bounds.Min.Y + bounds.Dy()*row/hashHeight
			y1 := bounds.Min.Y + bounds.Dy()*(row+1)/hashHeight
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}

			sum, count := 0.0, 0.0
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, b, _ := img.At(x, y).RGBA()
					sum += 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
					count++
				}
			}
			grid[row][col] = sum / count
		}
	}

	var hash uint64
	bit := 0
	for row := 0; row < hashHeight; row++ {
		for col := 0; col < hashWidth-1; col++ {
			if grid[row][col] > grid[row][col+1] {
				hash |= 1 << bit
			}
			bit++
		}
	}
	return hash
}

// Hasher downloads thumbnails and hashes them. It is used only on listings that are new,
// so a quiet pass downloads nothing at all.
type Hasher struct {
	HTTP  *http.Client
	Limit int
}

func NewHasher(limit int) *Hasher {
	return &Hasher{HTTP: &http.Client{Timeout: 15 * time.Second}, Limit: limit}
}

// Hashes fetches the first few photos of a listing and returns their hashes. A photo that
// cannot be read is skipped: a missing hash costs a duplicate, never a wrong match.
func (h *Hasher) Hashes(ctx context.Context, urls []string) []uint64 {
	var hashes []uint64
	for i, url := range urls {
		if h.Limit > 0 && i >= h.Limit {
			break
		}
		hash, err := h.one(ctx, url)
		if err != nil || hash == 0 {
			continue
		}
		hashes = append(hashes, hash)
	}
	return hashes
}

func (h *Hasher) one(ctx context.Context, url string) (uint64, error) {
	// The small size is the one to hash: 320 px is plenty for 64 bits and a tenth of the
	// bytes of the big one.
	url = strings.Replace(url, "pictureSize=W800", "pictureSize=W320", 1)
	url = strings.Replace(url, "pictureSize=W640", "pictureSize=W320", 1)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("photo %s answered %d", url, resp.StatusCode)
	}

	img, _, err := image.Decode(io.LimitReader(resp.Body, MaxPhotoBytes))
	if err != nil {
		return 0, err
	}
	return Hash(img), nil
}
