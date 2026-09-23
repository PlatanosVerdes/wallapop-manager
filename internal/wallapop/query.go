package wallapop

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	ErrNotASearch = errors.New("no es la direccion de una busqueda de Wallapop")
	ErrNoFilter   = errors.New("esa busqueda no filtra nada: le falta un texto, una categoria o una marca")
)

// dropped are the parameters that describe how the page was reached rather than what is
// being looked for. distance is among them because the API ignores it: the radius that
// filters is distance_in_km.
var dropped = map[string]bool{
	"order_by":        true,
	"source":          true,
	"next_page":       true,
	"search_id":       true,
	"filters_source":  true,
	"saved_search_id": true,
	"distance":        true,
}

// FromWebURL turns the address of a search made on the web into the query the API
// answers. Both use the same parameter names, so this is mostly a filter.
func FromWebURL(raw string) (url.Values, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, ErrNotASearch
	}
	host := strings.ToLower(u.Hostname())
	if host != "wallapop.com" && !strings.HasSuffix(host, ".wallapop.com") {
		return nil, ErrNotASearch
	}
	if path := strings.TrimSuffix(u.Path, "/"); path != "/search" && path != "/app/search" {
		return nil, fmt.Errorf("%w: %s", ErrNotASearch, u.Path)
	}

	query := url.Values{}
	for key, values := range u.Query() {
		if dropped[key] || len(values) == 0 || values[0] == "" {
			continue
		}
		query.Set(key, values[0])
	}
	if query.Get("keywords") == "" && query.Get("category_id") == "" && query.Get("brand") == "" {
		return nil, ErrNoFilter
	}
	return Searchable(query), nil
}

// Searchable fixes the two parameters the API insists on: without source it answers 400,
// and newest is the order in which something new shows up first.
func Searchable(query url.Values) url.Values {
	out := url.Values{}
	for key, values := range query {
		out[key] = append([]string(nil), values...)
	}
	out.Set("order_by", "newest")
	out.Set("source", "saved_search")
	return out
}

// RadiusKm is the radius a query is limited to, or zero when it covers the whole country:
// a radius without a point to measure it from is ignored.
func RadiusKm(query url.Values) string {
	if query.Get("latitude") == "" || query.Get("longitude") == "" {
		return ""
	}
	return query.Get("distance_in_km")
}

// WebURL is the search as a person opens it.
func WebURL(query url.Values) string {
	shown := url.Values{}
	for key, values := range query {
		if key != "source" {
			shown[key] = values
		}
	}
	return DefaultWebURL + "/search?" + shown.Encode()
}
