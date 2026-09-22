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
	said     []string
}

func (r *recorder) Listing(_ context.Context, search wallapop.SavedSearch, item wallapop.SearchItem) error {
	r.listings = append(r.listings, search.Name()+"|"+item.Title)
	return nil
}

func (r *recorder) Say(_ context.Context, text string) error {
	r.said = append(r.said, text)
	return nil
}

// fakeWallapop answers the two calls a round makes, plus the photos.
type fakeWallapop struct {
	searches []wallapop.SavedSearch
	items    []wallapop.SearchItem
	queries  []string
	// url is where the fake listens, so photo links in the fixtures are absolute.
	url string
}

func (f *fakeWallapop) photo(name string) string { return f.url + "/" + name + ".jpg" }

func (f *fakeWallapop) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == wallapop.PathSavedSearches:
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "no bearer", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(f.searches)
		case r.URL.Path == wallapop.PathSearch:
			// The catalogue search must go out anonymous: a bearer here would tie the
			// watching to the account.
			if r.Header.Get("Authorization") != "" {
				http.Error(w, "the search carried a bearer", http.StatusBadRequest)
				return
			}
			f.queries = append(f.queries, r.URL.RawQuery)
			body := map[string]any{"data": map[string]any{"section": map[string]any{
				"payload": map[string]any{"items": f.items},
			}}}
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

func newSearch(id, title string, enabled bool) wallapop.SavedSearch {
	s := wallapop.SavedSearch{ID: id, Title: title, Query: map[string]any{"keywords": title, "max_sale_price": 200.0}}
	s.Alert.Enabled = enabled
	return s
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
	return client, Options{MaxAge: 24 * time.Hour, MaxAlerts: 10, PhotosPerItem: 1, SeenTTL: time.Hour}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The first round of a search is silence: everything on that page was already there.
func TestFirstRoundSeedsWithoutSpeaking(t *testing.T) {
	fake := &fakeWallapop{
		searches: []wallapop.SavedSearch{newSearch("s1", "kallax", true)},
	}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{
		newItem("a", "Estantería Kallax", 40, time.Minute, fake.photo("a")),
		newItem("b", "Kallax blanca 4 huecos", 60, time.Minute, fake.photo("b")),
	}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	res := Run(context.Background(), client, seen, notify, opt, quiet())

	if len(notify.listings) != 0 {
		t.Fatalf("the first round announced %v", notify.listings)
	}
	if res.Seeded != 2 || res.Watched != 1 {
		t.Fatalf("seeded=%d watched=%d, expected 2 and 1", res.Seeded, res.Watched)
	}

	// Second round, one genuinely new listing.
	fake.items = append(fake.items, newItem("c", "Kallax 2 puertas", 75, time.Minute, fake.photo("c")))
	res = Run(context.Background(), client, seen, notify, opt, quiet())
	if len(notify.listings) != 1 || !strings.HasSuffix(notify.listings[0], "Kallax 2 puertas") {
		t.Fatalf("the second round announced %v", notify.listings)
	}
	if len(res.New) != 1 {
		t.Fatalf("new = %d, expected 1", len(res.New))
	}
}

// A search whose alert is off in the app is not watched, and nothing turns it back on.
func TestAlertOffIsLeftAlone(t *testing.T) {
	fake := &fakeWallapop{searches: []wallapop.SavedSearch{
		newSearch("s1", "kallax", true),
		newSearch("s2", "bmw f800gs", false),
	}}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("a", "Something", 10, time.Minute, fake.photo("a"))}

	seen, _ := LoadSeen(t.TempDir())
	res := Run(context.Background(), client, seen, &recorder{}, opt, quiet())
	if res.Watched != 1 || res.Ignored != 1 {
		t.Fatalf("watched=%d ignored=%d, expected 1 and 1", res.Watched, res.Ignored)
	}
	if len(fake.queries) != 1 {
		t.Fatalf("%d searches were run, expected 1", len(fake.queries))
	}

	res = Run(context.Background(), client, seen, &recorder{}, Options{All: true, MaxAge: time.Hour, PhotosPerItem: 1}, quiet())
	if res.Watched != 2 {
		t.Fatalf("with --all, watched=%d, expected 2", res.Watched)
	}
}

// The same advert reposted by another account from another town is announced once.
func TestReposetdListingIsAnnouncedOnce(t *testing.T) {
	fake := &fakeWallapop{searches: []wallapop.SavedSearch{newSearch("s1", "motos", true)}}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("seed", "Otra moto cualquiera", 3000, time.Minute, fake.photo("z"))}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	Run(context.Background(), client, seen, notify, opt, quiet())

	// Both carry the same photograph, and the price barely moves.
	first := newItem("m1", "YAMAHA XSR 900 (A2)", 8780, time.Minute, fake.photo("same"))
	second := newItem("m2", "Yamaha XSR900 A2 impecable", 8800, time.Minute, fake.photo("same"))
	fake.items = append(fake.items, first, second)

	res := Run(context.Background(), client, seen, notify, opt, quiet())
	if len(notify.listings) != 1 {
		t.Fatalf("announced %v, expected one of the two copies", notify.listings)
	}
	if res.Duplicates != 1 {
		t.Fatalf("duplicates = %d, expected 1", res.Duplicates)
	}
}

func TestFloodIsCapped(t *testing.T) {
	fake := &fakeWallapop{searches: []wallapop.SavedSearch{newSearch("s1", "kallax", true)}}
	client, opt := newWatcher(t, fake)
	opt.MaxAlerts = 2
	fake.items = []wallapop.SearchItem{newItem("seed", "Algo", 10, time.Minute, fake.photo("seed"))}

	seen, _ := LoadSeen(t.TempDir())
	notify := &recorder{}
	Run(context.Background(), client, seen, notify, opt, quiet())

	for i := 0; i < 5; i++ {
		fake.items = append(fake.items, newItem(fmt.Sprintf("n%d", i),
			fmt.Sprintf("Cosa distinta numero %d", i), float64(100+i*40), time.Minute, fake.photo(fmt.Sprintf("p%d", i))))
	}
	res := Run(context.Background(), client, seen, notify, opt, quiet())

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

// A silenced search is not read at all: the app still has its alert on, this just has
// nothing to say about it.
func TestSilencedSearchIsSkipped(t *testing.T) {
	fake := &fakeWallapop{searches: []wallapop.SavedSearch{
		newSearch("s1", "kallax", true),
		newSearch("s2", "motos", true),
	}}
	client, opt := newWatcher(t, fake)
	fake.items = []wallapop.SearchItem{newItem("a", "Algo nuevo", 10, time.Minute, fake.photo("a"))}

	mutes, err := LoadMutes(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutes.Toggle("s2", "motos"); err != nil {
		t.Fatal(err)
	}
	opt.Mutes = mutes

	seen, _ := LoadSeen(t.TempDir())
	res := Run(context.Background(), client, seen, &recorder{}, opt, quiet())

	if res.Watched != 1 || res.Silenced != 1 {
		t.Fatalf("watched=%d silenced=%d, expected 1 and 1", res.Watched, res.Silenced)
	}
	if len(fake.queries) != 1 {
		t.Fatalf("%d searches were run, expected only the one that is not silenced", len(fake.queries))
	}
}
