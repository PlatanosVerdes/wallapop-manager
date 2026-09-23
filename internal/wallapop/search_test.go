package wallapop

import (
	"errors"
	"testing"
)

func TestFromWebURL(t *testing.T) {
	query, err := FromWebURL("https://es.wallapop.com/search?brand=Yamaha&category_id=14000&model=XSR+900" +
		"&min_year=2022&latitude=41.38&longitude=2.13&distance=10000&distance_in_km=50" +
		"&order_by=most_relevance&source=search_box&filters_source=quick_filters")
	if err != nil {
		t.Fatal(err)
	}
	if got := query.Get("order_by"); got != "newest" {
		t.Errorf("order_by = %q, expected newest", got)
	}
	// Without source the API answers 400.
	if got := query.Get("source"); got == "" {
		t.Error("the query went out without a source")
	}
	if got := query.Get("model"); got != "XSR 900" {
		t.Errorf("model = %q", got)
	}
	for _, key := range []string{"distance", "filters_source"} {
		if query.Has(key) {
			t.Errorf("%s was kept", key)
		}
	}
	if got := RadiusKm(query); got != "50" {
		t.Errorf("radius = %q, expected 50", got)
	}
}

func TestFromWebURLRefusesWhatIsNotASearch(t *testing.T) {
	for _, raw := range []string{
		"hola",
		"https://es.wallapop.com/item/yamaha-xsr-900-2024-1304957296",
		"https://evil.example/search?keywords=kallax",
		"https://wallapop.com.evil.example/search?keywords=kallax",
	} {
		if _, err := FromWebURL(raw); !errors.Is(err, ErrNotASearch) {
			t.Errorf("FromWebURL(%q) = %v, expected ErrNotASearch", raw, err)
		}
	}
	if _, err := FromWebURL("https://es.wallapop.com/search?latitude=41.38&longitude=2.13"); !errors.Is(err, ErrNoFilter) {
		t.Errorf("a search with no filter was accepted: %v", err)
	}
	if _, err := FromWebURL("https://es.wallapop.com/app/search?keywords=kallax"); err != nil {
		t.Errorf("the old address was refused: %v", err)
	}
}

// A radius needs a point to be measured from, and without one the API covers the country.
func TestRadiusNeedsAPoint(t *testing.T) {
	query, err := FromWebURL("https://es.wallapop.com/search?keywords=kallax&distance_in_km=5")
	if err != nil {
		t.Fatal(err)
	}
	if got := RadiusKm(query); got != "" {
		t.Errorf("radius = %q without coordinates", got)
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
