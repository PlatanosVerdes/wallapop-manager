package wallapop

import "testing"

// Shaped like the real answer: the query carries Wallapop's own parameter names, numbers
// come back as JSON numbers, and the alert is the switch shown in the app.
const savedSearchesFixture = `[
  {"id": "c6ae82bf", "title": "Motos",
   "query": {"latitude": 41.383, "longitude": 2.134, "category_id": "14000",
             "order_by": "closest", "country_code": "ES", "brand": "Yamaha",
             "model": "XSR 900", "min_year": 2022, "saved_search_id": "c6ae82bf"},
   "alert": {"enabled": true, "hits": 0, "distance": 10000},
   "location_label": "08028 Barcelona"},
  {"id": "eeee9d73", "title": "kallax",
   "query": {"keywords": "kallax", "max_sale_price": 200.0, "distance_in_km": 1},
   "alert": {"enabled": false},
   "location_label": "08028 Barcelona"}
]`

func TestDecodeSearches(t *testing.T) {
	searches, err := decodeSearches([]byte(savedSearchesFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(searches) != 2 {
		t.Fatalf("expected 2 searches, got %d", len(searches))
	}
	if !searches[0].Alert.Enabled || searches[1].Alert.Enabled {
		t.Error("the alert switch did not decode")
	}
	if searches[0].Name() != "Motos" {
		t.Errorf("name = %q", searches[0].Name())
	}
}

func TestSavedSearchValues(t *testing.T) {
	searches, err := decodeSearches([]byte(savedSearchesFixture))
	if err != nil {
		t.Fatal(err)
	}
	query := searches[0].Values()

	// Newest is the whole point: the round is about what has just appeared.
	if got := query.Get("order_by"); got != "newest" {
		t.Errorf("order_by = %q, expected newest", got)
	}
	if query.Has("saved_search_id") {
		t.Error("the bookkeeping id was sent to the search endpoint")
	}
	// A whole number must not arrive as 2022.000000.
	if got := query.Get("min_year"); got != "2022" {
		t.Errorf("min_year = %q", got)
	}
	if got := query.Get("distance"); got != "10000" {
		t.Errorf("distance = %q, expected the alert's own radius", got)
	}
	if got := query.Get("model"); got != "XSR 900" {
		t.Errorf("model = %q", got)
	}

	second := searches[1].Values()
	if got := second.Get("max_sale_price"); got != "200" {
		t.Errorf("max_sale_price = %q", got)
	}
	if second.Has("distance") {
		t.Error("a search with no alert radius was given one")
	}
}

func TestItemURLAndPhoto(t *testing.T) {
	item := SearchItem{ID: "abc", WebSlug: "una-moto-123"}
	if got := item.URL(); got != ItemURL+"una-moto-123" {
		t.Errorf("url = %q", got)
	}
	if got := (SearchItem{ID: "abc"}).URL(); got != ItemURL+"abc" {
		t.Errorf("url without a slug = %q", got)
	}
	if got := item.Photo(); got != "" {
		t.Errorf("a listing with no images answered %q", got)
	}

	withImage := SearchItem{}
	withImage.Images = make([]Image, 1)
	withImage.Images[0].URLs.Medium = "medium.jpg"
	if got := withImage.Photo(); got != "medium.jpg" {
		t.Errorf("photo = %q, expected the medium one when there is no big", got)
	}
}
