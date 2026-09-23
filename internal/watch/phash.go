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

// dHash: a 9x8 grey grid, each bit says whether a cell is brighter than its right neighbour.
// Re-compression moves a bit or two, so hashes a few bits apart are the same photo.
const (
	hashWidth  = 9
	hashHeight = 8
	// Far above the ~30 KB thumbnail.
	MaxPhotoBytes = 2 << 20
)

func Distance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

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

// Hasher only runs on new listings, so a quiet pass downloads nothing.
type Hasher struct {
	HTTP  *http.Client
	Limit int
}

func NewHasher(limit int) *Hasher {
	return &Hasher{HTTP: &http.Client{Timeout: 15 * time.Second}, Limit: limit}
}

// An unreadable photo is skipped: a missing hash costs a duplicate, never a wrong match.
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
	// 320 px is plenty for 64 bits, at a tenth of the bytes.
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
