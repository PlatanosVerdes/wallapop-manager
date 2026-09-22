package watch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

func item(id, user, title string, price float64, city string) wallapop.SearchItem {
	it := wallapop.SearchItem{ID: id, UserID: user, Title: title}
	it.Price = wallapop.Price{Amount: price, Currency: "EUR"}
	it.Location.City = city
	return it
}

func TestTokenize(t *testing.T) {
	got := Tokenize("Vendo Estantería KALLAX, muy buen estado!! 2 puertas")
	want := []string{"2", "estanteria", "kallax", "puertas"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, expected %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens = %v, expected %v", got, want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	same := Similarity(Tokenize("Yamaha XSR 900 A2"), Tokenize("YAMAHA XSR 900 (A2)"))
	if same != 1 {
		t.Errorf("the same title with other punctuation scored %.2f", same)
	}
	if apart := Similarity(Tokenize("Yamaha XSR 900"), Tokenize("Estantería Kallax")); apart != 0 {
		t.Errorf("two unrelated titles scored %.2f", apart)
	}
}

// The case this exists for: one advert posted by several accounts from several towns at
// almost the same price. The photograph is what gives it away.
func TestDuplicateBySamePhoto(t *testing.T) {
	seen, _ := LoadSeen(t.TempDir())
	seen.Add(item("a", "seller-1", "YAMAHA XSR 900 (A2)", 8780, "Barcelona"), []uint64{0xdeadbeefcafe1234}, "motos", time.Now())

	// A copy: another account, another town, a price nudged, the same picture.
	copyOf := item("b", "seller-2", "Yamaha XSR900 A2 impecable", 8850, "Sevilla")
	rec, reason, dup := seen.Duplicate(copyOf, []uint64{0xdeadbeefcafe1236})
	if !dup {
		t.Fatal("the same photograph from another seller was not caught")
	}
	if reason != "same photo" || rec.ID != "a" {
		t.Fatalf("caught by %q against %s", reason, rec.ID)
	}
}

// Two strangers both using the maker's catalogue photo of the same white shelf are two
// shelves. The price is what separates them from a reposted advert.
func TestPhotoMatchNeedsAPlausiblePrice(t *testing.T) {
	seen, _ := LoadSeen(t.TempDir())
	seen.Add(item("a", "seller-1", "Estantería Kallax Ikea blanca", 16, "Madrid"), []uint64{0x0f0f0f0f0f0f0f0f}, "kallax", time.Now())

	other := item("b", "seller-2", "Mueble almacenaje Kallax", 40, "Sevilla")
	if _, _, dup := seen.Duplicate(other, []uint64{0x0f0f0f0f0f0f0f0f}); dup {
		t.Fatal("two different shelves sharing the catalogue photo were folded into one")
	}
}

func TestWordsOnlyFoldTheSameSeller(t *testing.T) {
	seen, _ := LoadSeen(t.TempDir())
	seen.Add(item("a", "seller-1", "Estantería Kallax Ikea", 40, "Sant Just"), nil, "kallax", time.Now())

	// Same seller, listing rewritten and repriced: his own thing again.
	his := item("b", "seller-1", "Estantería Kallax Ikea 2 puertas", 45, "Sant Just")
	if _, reason, dup := seen.Duplicate(his, nil); !dup || reason != "same seller, listing rewritten" {
		t.Fatalf("a seller reposting his own listing was not caught (%q)", reason)
	}

	// Word for word the same, from somebody else: a second identical shelf, not a copy.
	hers := item("c", "seller-2", "Estantería Kallax Ikea", 40, "Madrid")
	if _, _, dup := seen.Duplicate(hers, nil); dup {
		t.Fatal("two sellers with the same mass-produced thing were folded into one")
	}
}

func TestKnownAndWatched(t *testing.T) {
	dir := t.TempDir()
	seen, _ := LoadSeen(dir)
	seen.Add(item("a", "u", "Something", 10, "Madrid"), []uint64{1}, "search", time.Now())
	seen.MarkWatched("search-id", time.Now())
	if err := seen.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := LoadSeen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Known("a") {
		t.Error("the listing was not remembered across a restart")
	}
	if !again.Watched("search-id") {
		t.Error("the search was not remembered as already watched")
	}
	if again.Known("b") {
		t.Error("an unknown id came back as known")
	}
	if _, err := LoadSeen(filepath.Join(dir, "nope")); err != nil {
		t.Errorf("a missing store should start empty, not fail: %v", err)
	}
}

func TestPrune(t *testing.T) {
	seen, _ := LoadSeen(t.TempDir())
	now := time.Now()
	seen.Add(item("old", "u", "Old thing", 10, "Madrid"), nil, "s", now.Add(-40*24*time.Hour))
	seen.Add(item("new", "u", "New thing", 10, "Madrid"), nil, "s", now)

	seen.Prune(30*24*time.Hour, now)
	if seen.Known("old") {
		t.Error("a record past the ttl survived")
	}
	if !seen.Known("new") {
		t.Error("a recent record was pruned")
	}
}

func TestMoney(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		want   string
	}{
		{8780, "8.780 €"},
		{999, "999 €"},
		{1234567, "1.234.567 €"},
		{0, "0 €"},
	} {
		if got := Money(tc.amount, "EUR"); got != tc.want {
			t.Errorf("Money(%v) = %q, expected %q", tc.amount, got, tc.want)
		}
	}
}
