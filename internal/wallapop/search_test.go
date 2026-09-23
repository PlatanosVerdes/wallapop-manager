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

func TestFromText(t *testing.T) {
	cases := []struct {
		text, keywords, min, max, km string
	}{
		{"iphone 13", "iphone 13", "", "", ""},
		{"iphone 13 hasta 400", "iphone 13", "", "400", ""},
		{"bici 100-300€", "bici", "100", "300", ""},
		{"sofá entre 50 y 200 euros", "sofá", "50", "200", ""},
		{"Kallax máx 40", "Kallax", "", "40", ""},
		{"moto desde 1.500 a 30 km", "moto", "1500", "", "30"},
		{"ps5 menos de 300 20km", "ps5", "", "300", "20"},
	}
	for _, c := range cases {
		query, err := FromText(c.text)
		if err != nil {
			t.Errorf("FromText(%q): %v", c.text, err)
			continue
		}
		got := []string{query.Get("keywords"), query.Get("min_sale_price"), query.Get("max_sale_price"), query.Get("distance_in_km")}
		want := []string{c.keywords, c.min, c.max, c.km}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("FromText(%q) = %q, expected %q", c.text, got, want)
				break
			}
		}
		if query.Get("source") == "" {
			t.Errorf("FromText(%q) is not searchable", c.text)
		}
	}
	if _, err := FromText("hasta 400"); !errors.Is(err, ErrNoFilter) {
		t.Errorf("a price alone was taken as a search: %v", err)
	}
}

func TestNearKeepsTheRadiusAsked(t *testing.T) {
	query, _ := FromText("moto a 10 km")
	if got := Near(query, 41.38, 2.17, 30); got.Get("distance_in_km") != "10" || RadiusKm(got) != "10" {
		t.Errorf("radius = %q", got.Get("distance_in_km"))
	}
	query, _ = FromText("moto")
	if got := Near(query, 41.38, 2.17, 30); RadiusKm(got) != "30" || got.Get("latitude") != "41.38000" {
		t.Errorf("near = %v", got)
	}
}
