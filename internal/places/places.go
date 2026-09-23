// Package places turns a town name into coordinates.
package places

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is OpenStreetMap's geocoder: no account, one request a second.
const DefaultURL = "https://nominatim.openstreetmap.org"

type Place struct {
	Name      string
	Latitude  float64
	Longitude float64
}

type Finder struct {
	URL  string
	HTTP *http.Client
}

func New(base string) *Finder {
	return &Finder{URL: base, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (f *Finder) Enabled() bool { return f != nil && f.URL != "" }

// Find answers false for a street or a shop: only towns, districts and provinces count.
func (f *Finder) Find(ctx context.Context, name string) (Place, bool, error) {
	form := url.Values{
		"q":               {name},
		"countrycodes":    {"es"},
		"format":          {"json"},
		"addressdetails":  {"1"},
		"limit":           {"1"},
		"accept-language": {"es"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(f.URL, "/")+"/search?"+form.Encode(), nil)
	if err != nil {
		return Place{}, false, err
	}
	// Nominatim refuses requests that do not say who is asking.
	req.Header.Set("User-Agent", "wallapop-manager (github.com/PlatanosVerdes/wallapop-manager)")

	resp, err := f.HTTP.Do(req)
	if err != nil {
		return Place{}, false, fmt.Errorf("looking up %q: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Place{}, false, fmt.Errorf("looking up %q: the geocoder answered %d", name, resp.StatusCode)
	}

	var found []struct {
		Name    string `json:"name"`
		Class   string `json:"class"`
		Lat     string `json:"lat"`
		Lon     string `json:"lon"`
		Address struct {
			Province string `json:"province"`
			State    string `json:"state"`
		} `json:"address"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
		return Place{}, false, fmt.Errorf("looking up %q: %w", name, err)
	}
	if len(found) == 0 || (found[0].Class != "boundary" && found[0].Class != "place") {
		return Place{}, false, nil
	}
	hit := found[0]
	lat, errLat := strconv.ParseFloat(hit.Lat, 64)
	lon, errLon := strconv.ParseFloat(hit.Lon, 64)
	if errLat != nil || errLon != nil {
		return Place{}, false, fmt.Errorf("looking up %q: coordinates %q, %q", name, hit.Lat, hit.Lon)
	}
	label := hit.Name
	if region := firstOf(hit.Address.Province, hit.Address.State); region != "" && region != hit.Name {
		label += ", " + region
	}
	return Place{Name: label, Latitude: lat, Longitude: lon}, true, nil
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
