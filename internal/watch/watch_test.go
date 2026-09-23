package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

// stubSession is a session that is always fresh: the watcher's own calls are what is under
// test, not the renewal, which has its own tests.
type stubSession struct{}

func (stubSession) AccessToken() string                    { return "token" }
func (stubSession) SessionCookie() (string, string)        { return "cookie", "value" }
func (stubSession) Update(string, string, time.Time) error { return nil }

type recorder struct {
	listings []string
	cheaper  []string
	said     []string
}

func (r *recorder) Listing(_ context.Context, search Search, item wallapop.SearchItem) error {
	r.listings = append(r.listings, search.Name+"|"+item.Title)
	return nil
}

func (r *recorder) Cheaper(_ context.Context, search Search, item wallapop.SearchItem, before float64) error {
	r.cheaper = append(r.cheaper, fmt.Sprintf("%s|%s|%.0f→%.0f", search.Name, item.Title, before, item.Price.Amount))
	return nil
}

func (r *recorder) Say(_ context.Context, text string) error {
	r.said = append(r.said, text)
	return nil
}

// fakeWallapop answers the search a round makes, plus the photos.
type fakeWallapop struct {
	items   []wallapop.SearchItem
	queries []string
	// secondPage, when set, is served behind a cursor the way a long search answers.
	secondPage []wallapop.SearchItem
	// url is where the fake listens, so photo links in the fixtures are absolute.
	url string
}

func (f *fakeWallapop) photo(name string) string { return f.url + "/" + name + ".jpg" }

func (f *fakeWallapop) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == wallapop.PathSearch:
			// The catalogue search must go out anonymous: a bearer here would tie the
			// watching to the account.
			if r.Header.Get("Authorization") != "" {
				http.Error(w, "the search carried a bearer", http.StatusBadRequest)
				return
			}
			f.queries = append(f.queries, r.URL.RawQuery)
			items, next := f.items, ""
			if f.secondPage != nil {
				if r.URL.Query().Get("next_page") == "" {
					next = "cursor"
				} else {
					items = f.secondPage
				}
			}
			body := map[string]any{
				"data": map[string]any{"section": map[string]any{
					"payload": map[string]any{"items": items},
				}},
				"meta": map[string]any{"next_page": next},
			}
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			// One picture per name, so two listings pointing at the same file are the
			// only ones that hash alike.
			seed := 0
			for _, c := range r.URL.Path {
				seed += int(c)
			}
			_ = jpeg.Encode(w, gradient(320, 240, seed), nil)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return srv
}

func newSearch(id, title string) Search {
	return Search{ID: id, Name: title, Query: wallapop.Searchable(url.Values{"keywords": {title}})}
}

func newItem(id, title string, price float64, age time.Duration, photo string) wallapop.SearchItem {
	it := wallapop.SearchItem{ID: id, UserID: "seller-" + id, Title: title, WebSlug: "slug-" + id,
		CreatedAt: time.Now().Add(-age).UnixMilli()}
	it.Price = wallapop.Price{Amount: price, Currency: "EUR"}
	it.Images = []wallapop.Image{{}}
	it.Images[0].URLs.Small = photo
	return it
}

func newWatcher(t *testing.T, fake *fakeWallapop) (*wallapop.Client, Options) {
	srv := fake.server(t)
	client := wallapop.New(stubSession{})
	client.BaseURL = srv.URL
	return client, Options{MaxAge: 24 * time.Hour, MaxAlerts: 10, PhotosPerItem: 1, SeenTTL: time.Hour, Pages: 3}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The first round of a search is silence: everything on that page was already there.
func TestFirstRoundSeedsWithoutSpeaking(t *testing.T) {
	searches := []Search{newSearch("s1", "kallax")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{
		newItem("a", "Estantería Kallax", 40, time.Minute, fake.photo("a")),
		newItem("b", "Kallax blanca 4 huecos", 60, time.Minute, fake.photo("b")),
	}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	res := Run(context.Background(), client, seen, searches, notify, opt, quiet())

	if len(notify.listings) != 0 {
		t.Fatalf("the first round announced %v", notify.listings)
	}
	if res.Seeded != 2 || res.Watched != 1 {
		t.Fatalf("seeded=%d watched=%d, expected 2 and 1", res.Seeded, res.Watched)
	}

	// Second round, one genuinely new listing.
	fake.items = append(fake.items, newItem("c", "Kallax 2 puertas", 75, time.Minute, fake.photo("c")))
	res = Run(context.Background(), client, seen, searches, notify, opt, quiet())
	if len(notify.listings) != 1 || !strings.HasSuffix(notify.listings[0], "Kallax 2 puertas") {
		t.Fatalf("the second round announced %v", notify.listings)
	}
	if len(res.New) != 1 {
		t.Fatalf("new = %d, expected 1", len(res.New))
	}
}

// The same advert reposted by another account from another town is announced once.
func TestReposetdListingIsAnnouncedOnce(t *testing.T) {
	searches := []Search{newSearch("s1", "motos")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("seed", "Otra moto cualquiera", 3000, time.Minute, fake.photo("z"))}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())

	// Both carry the same photograph, and the price barely moves.
	first := newItem("m1", "YAMAHA XSR 900 (A2)", 8780, time.Minute, fake.photo("same"))
	second := newItem("m2", "Yamaha XSR900 A2 impecable", 8800, time.Minute, fake.photo("same"))
	fake.items = append(fake.items, first, second)

	res := Run(context.Background(), client, seen, searches, notify, opt, quiet())
	if len(notify.listings) != 1 {
		t.Fatalf("announced %v, expected one of the two copies", notify.listings)
	}
	if res.Duplicates != 1 {
		t.Fatalf("duplicates = %d, expected 1", res.Duplicates)
	}
}

func TestFloodIsCapped(t *testing.T) {
	searches := []Search{newSearch("s1", "kallax")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	opt.MaxAlerts = 2
	fake.items = []wallapop.SearchItem{newItem("seed", "Algo", 10, time.Minute, fake.photo("seed"))}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())

	for i := 0; i < 5; i++ {
		fake.items = append(fake.items, newItem(fmt.Sprintf("n%d", i),
			fmt.Sprintf("Cosa distinta numero %d", i), float64(100+i*40), time.Minute, fake.photo(fmt.Sprintf("p%d", i))))
	}
	res := Run(context.Background(), client, seen, searches, notify, opt, quiet())

	if len(notify.listings) != 2 {
		t.Fatalf("sent %d messages, expected the cap of 2", len(notify.listings))
	}
	if res.Held != 3 {
		t.Fatalf("held = %d, expected 3", res.Held)
	}
	if len(notify.said) != 1 || !strings.Contains(notify.said[0], "3 anuncios") {
		t.Fatalf("the tail message was %v", notify.said)
	}
}

func TestLine(t *testing.T) {
	item := newItem("a", "Kallax <barato> & bonito", 8780, time.Minute, "http://example.test/a.jpg")
	item.Location.City = "Barcelona"
	got := Line("kallax", item, func(s string) string { return strings.ReplaceAll(s, "<", "&lt;") })

	for _, want := range []string{"&lt;barato", "8.780 €", "Barcelona", "kallax"} {
		if !strings.Contains(got, want) {
			t.Errorf("the message does not carry %q:\n%s", want, got)
		}
	}
}

// A listing already seen is not news, but the same thing cheaper than it has ever been is.
func TestPriceDropIsAnnouncedOnce(t *testing.T) {
	searches := []Search{newSearch("s1", "motos")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	opt.Drop = 0.05
	bike := newItem("m1", "Yamaha XSR900", 9000, time.Minute, fake.photo("m1"))
	fake.items = []wallapop.SearchItem{bike}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())

	// Down 8%: worth saying.
	bike.Price.Amount = 8300
	fake.items = []wallapop.SearchItem{bike}
	res := Run(context.Background(), client, seen, searches, notify, opt, quiet())
	if len(notify.cheaper) != 1 || !strings.Contains(notify.cheaper[0], "9000→8300") {
		t.Fatalf("the drop was announced as %v", notify.cheaper)
	}
	if len(res.Cheaper) != 1 {
		t.Fatalf("res.Cheaper = %d", len(res.Cheaper))
	}

	// The same price again is the same news, and news is told once.
	Run(context.Background(), client, seen, searches, notify, opt, quiet())
	if len(notify.cheaper) != 1 {
		t.Fatalf("the same drop was announced twice: %v", notify.cheaper)
	}

	// Back up and down again to where it already was: still the same news.
	bike.Price.Amount = 9000
	fake.items = []wallapop.SearchItem{bike}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())
	bike.Price.Amount = 8300
	fake.items = []wallapop.SearchItem{bike}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())
	if len(notify.cheaper) != 1 {
		t.Fatalf("a price bouncing back to a known low was announced again: %v", notify.cheaper)
	}
}

func TestSmallDropIsNotWorthAMessage(t *testing.T) {
	searches := []Search{newSearch("s1", "motos")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	opt.Drop = 0.05
	bike := newItem("m1", "Yamaha XSR900", 9000, time.Minute, fake.photo("m1"))
	fake.items = []wallapop.SearchItem{bike}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())

	bike.Price.Amount = 8800 // 2.2%
	fake.items = []wallapop.SearchItem{bike}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())
	if len(notify.cheaper) != 0 {
		t.Fatalf("a 2%% haircut was announced: %v", notify.cheaper)
	}
}

// Eleven accounts repricing one van is one piece of news, and it belongs to the listing
// that was announced.
func TestACopyDropsInSilence(t *testing.T) {
	searches := []Search{newSearch("s1", "motos")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	opt.Drop = 0.05

	first := newItem("m1", "YAMAHA XSR 900 (A2)", 8780, time.Minute, fake.photo("same"))
	copyOf := newItem("m2", "Yamaha XSR900 A2 impecable", 8800, time.Minute, fake.photo("same"))
	fake.items = []wallapop.SearchItem{first, copyOf}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	res := Run(context.Background(), client, seen, searches, notify, opt, quiet())
	if res.Duplicates != 1 {
		t.Fatalf("the copy was not folded: %+v", res)
	}

	// Both drop, as a dealer network does.
	first.Price.Amount = 7900
	copyOf.Price.Amount = 7900
	fake.items = []wallapop.SearchItem{first, copyOf}
	Run(context.Background(), client, seen, searches, notify, opt, quiet())

	if len(notify.cheaper) != 1 {
		t.Fatalf("expected one message for the drop, got %v", notify.cheaper)
	}
	if !strings.Contains(notify.cheaper[0], "YAMAHA XSR 900 (A2)") {
		t.Fatalf("the copy spoke instead of the listing that was announced: %v", notify.cheaper)
	}
}

// A page is 40 listings and a search can hold more: the tail has to be read too, or
// the listings in it are never seen to change price.
func TestSearchFollowsTheCursor(t *testing.T) {
	searches := []Search{newSearch("s1", "motos")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("a", "Primera pagina", 100, time.Minute, fake.photo("a"))}
	fake.secondPage = []wallapop.SearchItem{newItem("b", "Segunda pagina", 200, time.Minute, fake.photo("b"))}

	seen, _ := LoadSeen(t.TempDir())
	res := Run(context.Background(), client, seen, searches, &recorder{}, opt, quiet())

	if res.Scanned != 2 {
		t.Fatalf("scanned = %d, expected both pages", res.Scanned)
	}
	if !seen.Known("b") {
		t.Error("the listing on the second page was never read")
	}
}

// Between deep rounds only the first page is read: the search is ordered by newest, so
// anything new is on it, and the other pages are requests for listings already known.
func TestOnlyTheFirstPageIsReadBetweenDeepRounds(t *testing.T) {
	searches := []Search{newSearch("s1", "motos")}
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("a", "Primera pagina", 100, time.Minute, fake.photo("a"))}
	fake.secondPage = []wallapop.SearchItem{newItem("b", "Segunda pagina", 200, time.Minute, fake.photo("b"))}

	seen, _ := LoadSeen(t.TempDir())
	Run(context.Background(), client, seen, searches, &recorder{}, opt, quiet())
	if len(fake.queries) != 2 {
		t.Fatalf("the first round of a search read %d pages, expected all of them", len(fake.queries))
	}

	fake.queries = nil
	Run(context.Background(), client, seen, searches, &recorder{}, opt, quiet())
	if len(fake.queries) != 1 {
		t.Fatalf("a shallow round read %d pages, expected 1", len(fake.queries))
	}

	fake.queries = nil
	opt.Deep = true
	Run(context.Background(), client, seen, searches, &recorder{}, opt, quiet())
	if len(fake.queries) != 2 {
		t.Fatalf("a deep round read %d pages, expected all of them", len(fake.queries))
	}
}

// Two chats watching the same search cost one request, and each still hears about the
// listing, because each has its own memory of what it has seen.
func TestTheSameSearchIsAskedOncePerRound(t *testing.T) {
	fake := &fakeWallapop{}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("a", "Estanteria", 40, time.Minute, fake.photo("a"))}

	ana, _ := LoadSeen(t.TempDir())
	luis, _ := LoadSeen(t.TempDir())
	searches := []Search{newSearch("s1", "kallax")}
	Run(context.Background(), client, ana, searches, &recorder{}, opt, quiet())
	Run(context.Background(), client, luis, []Search{newSearch("s9", "kallax")}, &recorder{}, opt, quiet())

	fake.queries = nil
	fake.items = append(fake.items, newItem("b", "Kallax nueva", 60, time.Minute, fake.photo("b")))
	catalogue := NewCatalogue(client, 0, 0)
	toAna, toLuis := &recorder{}, &recorder{}
	Run(context.Background(), catalogue, ana, searches, toAna, opt, quiet())
	Run(context.Background(), catalogue, luis, []Search{newSearch("s9", "kallax")}, toLuis, opt, quiet())

	if len(fake.queries) != 1 || catalogue.Requests != 1 || catalogue.Shared != 1 {
		t.Fatalf("%d requests went out (%d shared), expected 1", len(fake.queries), catalogue.Shared)
	}
	if len(toAna.listings) != 1 || len(toLuis.listings) != 1 {
		t.Fatalf("ana heard %v and luis %v, expected one listing each", toAna.listings, toLuis.listings)
	}
}

// A shallow answer cannot stand in for a deep one: the chat that needs every page asks.
func TestADeeperReadIsNotAnsweredByAShallowOne(t *testing.T) {
	fake := &fakeWallapop{}
	client, _ := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("a", "Primera", 100, time.Minute, fake.photo("a"))}
	fake.secondPage = []wallapop.SearchItem{newItem("b", "Segunda", 200, time.Minute, fake.photo("b"))}

	catalogue := NewCatalogue(client, 0, 0)
	query := newSearch("s1", "motos").Query
	if items, _ := catalogue.Search(context.Background(), query, 1); len(items) != 1 {
		t.Fatalf("one page answered %d listings", len(items))
	}
	if items, _ := catalogue.Search(context.Background(), query, 3); len(items) != 2 {
		t.Fatalf("the deep read answered %d listings, expected both pages", len(items))
	}
	if items, _ := catalogue.Search(context.Background(), query, 1); len(items) != 2 || catalogue.Shared != 1 {
		t.Fatalf("the shallow read after a deep one went out again: shared=%d", catalogue.Shared)
	}
}
