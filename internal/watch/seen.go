// Package watch follows the searches and announces what is new.
package watch

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

// Dealers repost one vehicle from many accounts, so the photo identifies it, not the title.
// Measured over 5.886 pairs: 1.5% sat under 10 bits apart, the rest above 20.
const (
	SamePhoto = 10
	// Two sellers with the same catalogue photo are two items, and their prices differ.
	samePhotoPrice       = 0.10
	samePhotoPriceSeller = 0.50

	// Words only match within one seller: strangers write the same title for different items.
	sameSeller      = 0.6
	sameSellerPrice = 0.30
)

type Record struct {
	ID     string   `json:"id"`
	UserID string   `json:"user_id"`
	Title  string   `json:"title"`
	Tokens []string `json:"tokens"`
	Price  float64  `json:"price"`
	// Drops are measured against the lowest price, so a price bouncing back and forth is announced once.
	Lowest float64  `json:"lowest,omitempty"`
	Hashes []uint64 `json:"hashes,omitempty"`
	City   string   `json:"city,omitempty"`
	Search string   `json:"search,omitempty"`
	// A copy is never announced, not even its price drops.
	CopyOf    string    `json:"copy_of,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
}

type Seen struct {
	Searches map[string]time.Time `json:"searches"`
	Records  []Record             `json:"records"`

	path  string
	index map[string]int
}

func LoadSeen(dir string) (*Seen, error) {
	seen := &Seen{
		Searches: map[string]time.Time{},
		path:     filepath.Join(dir, "seen.json"),
		index:    map[string]int{},
	}
	raw, err := os.ReadFile(seen.path)
	if os.IsNotExist(err) {
		return seen, nil
	}
	if err != nil {
		return seen, err
	}
	if err := json.Unmarshal(raw, seen); err != nil {
		return seen, err
	}
	if seen.Searches == nil {
		seen.Searches = map[string]time.Time{}
	}
	seen.reindex()
	return seen, nil
}

func (s *Seen) reindex() {
	s.index = make(map[string]int, len(s.Records))
	for i, rec := range s.Records {
		s.index[rec.ID] = i
	}
}

func (s *Seen) Known(id string) bool {
	_, ok := s.index[id]
	return ok
}

// The first pass of a new search announces nothing: its first page is old news.
func (s *Seen) Watched(searchID string) bool {
	_, ok := s.Searches[searchID]
	return ok
}

func (s *Seen) MarkWatched(searchID string, at time.Time) {
	if _, ok := s.Searches[searchID]; !ok {
		s.Searches[searchID] = at
	}
}

// Duplicate returns the reason too, so a dry run can say which rule matched.
func (s *Seen) Duplicate(item wallapop.SearchItem, hashes []uint64) (Record, string, bool) {
	tokens := Tokenize(item.Title)
	price := item.Price.Amount

	for _, rec := range s.Records {
		if rec.ID == item.ID {
			continue
		}
		tolerance := samePhotoPrice
		if rec.UserID != "" && rec.UserID == item.UserID {
			tolerance = samePhotoPriceSeller
		}
		if closestPhoto(hashes, rec.Hashes) <= SamePhoto && closePrice(price, rec.Price, tolerance) {
			return rec, "same photo", true
		}
	}

	if len(tokens) == 0 {
		return Record{}, "", false
	}
	best, bestReason, score := Record{}, "", 0.0
	for _, rec := range s.Records {
		if rec.ID == item.ID {
			continue
		}
		similarity := Similarity(tokens, rec.Tokens)
		if similarity <= score {
			continue
		}
		if rec.UserID != "" && rec.UserID == item.UserID &&
			similarity >= sameSeller && closePrice(price, rec.Price, sameSellerPrice) {
			best, bestReason, score = rec, "same seller, listing rewritten", similarity
		}
	}
	return best, bestReason, bestReason != ""
}

// No usable hash answers 65, which no threshold accepts.
func closestPhoto(a, b []uint64) int {
	closest := 65
	for _, one := range a {
		for _, other := range b {
			if d := Distance(one, other); d < closest {
				closest = d
			}
		}
	}
	return closest
}

func (s *Seen) Add(item wallapop.SearchItem, hashes []uint64, search string, at time.Time) {
	s.add(item, hashes, search, "", at)
}

func (s *Seen) AddCopy(item wallapop.SearchItem, hashes []uint64, search, copyOf string, at time.Time) {
	s.add(item, hashes, search, copyOf, at)
}

func (s *Seen) add(item wallapop.SearchItem, hashes []uint64, search, copyOf string, at time.Time) {
	if i, ok := s.index[item.ID]; ok {
		s.Records[i].Search = search
		return
	}
	s.Records = append(s.Records, Record{
		ID:        item.ID,
		UserID:    item.UserID,
		Title:     item.Title,
		Tokens:    Tokenize(item.Title),
		Price:     item.Price.Amount,
		Lowest:    item.Price.Amount,
		Hashes:    hashes,
		City:      item.Where(),
		Search:    search,
		CopyOf:    copyOf,
		FirstSeen: at,
	})
	s.index[item.ID] = len(s.Records) - 1
}

// The stored price is refreshed either way, so the same drop is never announced twice.
func (s *Seen) Cheaper(item wallapop.SearchItem, drop float64) (before float64, worth bool) {
	i, ok := s.index[item.ID]
	if !ok {
		return 0, false
	}
	rec := &s.Records[i]

	now := item.Price.Amount
	was := rec.Lowest
	if was == 0 {
		was = rec.Price
	}
	defer func() {
		rec.Price = now
		if now < rec.Lowest || rec.Lowest == 0 {
			rec.Lowest = now
		}
	}()

	if rec.CopyOf != "" || now <= 0 || was <= 0 || now >= was {
		return was, false
	}
	return was, (was-now)/was >= drop
}

func (s *Seen) Prune(ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	cut := now.Add(-ttl)
	kept := s.Records[:0]
	for _, rec := range s.Records {
		if rec.FirstSeen.After(cut) {
			kept = append(kept, rec)
		}
	}
	s.Records = kept
	s.reindex()
}

func (s *Seen) Save() error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// A missing price never rules a match out.
func closePrice(a, b, tolerance float64) bool {
	if a <= 0 || b <= 0 {
		return true
	}
	return math.Abs(a-b)/math.Max(a, b) <= tolerance
}

func Similarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]bool, len(a))
	for _, token := range a {
		set[token] = true
	}
	shared := 0
	for _, token := range b {
		if set[token] {
			shared++
		}
	}
	union := len(set) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// filler words make unrelated listings look alike.
var filler = map[string]bool{
	"vendo": true, "venta": true, "nuevo": true, "nueva": true, "seminuevo": true,
	"con": true, "sin": true, "por": true, "para": true, "del": true, "las": true,
	"los": true, "una": true, "uno": true, "muy": true, "buen": true, "buena": true,
	"estado": true, "mas": true, "que": true, "año": true, "ano": true, "anos": true,
}

func Tokenize(title string) []string {
	var builder strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(unaccent(r))
		default:
			builder.WriteRune(' ')
		}
	}

	unique := map[string]bool{}
	var tokens []string
	for _, token := range strings.Fields(builder.String()) {
		// A lone digit matters: "2 puertas" is not "4 puertas".
		if filler[token] || unique[token] || (len(token) < 2 && !unicode.IsDigit(rune(token[0]))) {
			continue
		}
		unique[token] = true
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	return tokens
}

func unaccent(r rune) rune {
	switch r {
	case 'á', 'à', 'ä', 'â':
		return 'a'
	case 'é', 'è', 'ë', 'ê':
		return 'e'
	case 'í', 'ì', 'ï', 'î':
		return 'i'
	case 'ó', 'ò', 'ö', 'ô':
		return 'o'
	case 'ú', 'ù', 'ü', 'û':
		return 'u'
	case 'ñ':
		return 'n'
	case 'ç':
		return 'c'
	}
	return r
}
