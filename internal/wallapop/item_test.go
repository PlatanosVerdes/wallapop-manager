package wallapop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestItemSlug(t *testing.T) {
	for raw, want := range map[string]string{
		"https://wallapop.com/item/kallax-negra-1305066398":     "kallax-negra-1305066398",
		"https://es.wallapop.com/item/kallax-negra-1305066398/": "kallax-negra-1305066398",
	} {
		if got, ok := ItemSlug(raw); !ok || got != want {
			t.Errorf("ItemSlug(%q) = %q, %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"https://es.wallapop.com/search?keywords=kallax", "https://example.com/item/x"} {
		if _, ok := ItemSlug(raw); ok {
			t.Errorf("ItemSlug(%q) took it as a listing", raw)
		}
	}
}

// The address carries the slug, the page the id, and the API the rest.
func TestListingMakesASearchLikeIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item/kallax-negra-1305066398":
			_, _ = w.Write([]byte(`<script>{"props":{"pageProps":{"item":{"id":"pzpk0nxpkmj3","title":{}}}}}</script>`))
		case "/api/v3/items/pzpk0nxpkmj3":
			_, _ = w.Write([]byte(`{"id":"pzpk0nxpkmj3","title":{"original":"Estantería IKEA Kallax 3x4 Negra"},
				"taxonomy":[{"id":"12467"},{"id":"10125"}],"price":{"cash":{"amount":40.0}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := New(nil)
	client.BaseURL, client.WebURL = server.URL, server.URL

	listing, err := client.Listing(context.Background(), "kallax-negra-1305066398")
	if err != nil {
		t.Fatal(err)
	}
	query := LikeListing(listing)
	if query.Get("keywords") != "Estantería IKEA Kallax" || query.Get("category_id") != "12467" || query.Get("max_sale_price") != "48" {
		t.Errorf("query = %v", query)
	}
	if _, err := client.Listing(context.Background(), "gone-1"); err != ErrNoListing {
		t.Errorf("a missing listing gave %v", err)
	}
}
