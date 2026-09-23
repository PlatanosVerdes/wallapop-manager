package wallapop

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	ErrNotASearch = errors.New("eso no es el enlace de una búsqueda de Wallapop")
	ErrNoFilter   = errors.New("esa búsqueda no filtra nada: ponle un texto, una categoría o una marca")
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

// A price is written the Spanish way, so 1.200 is a thousand two hundred.
const price = `(\d{1,3}(?:\.\d{3})+|\d+)\s*(?:€|euros?)?`

var (
	priceBetween = regexp.MustCompile(`(?i)(?:entre\s+)?\b` + price + `\s*(?:-|\by\b)\s*` + price)
	priceUpTo    = regexp.MustCompile(`(?i)(?:\bhasta|\bmax\.?|\bmáx\.?|\bmáximo|\bmaximo|\bmenos de|<)\s*` + price)
	priceFrom    = regexp.MustCompile(`(?i)(?:\bdesde|\bmin\.?|\bmín\.?|\bmínimo|\bminimo|\bmás de|\bmas de|>)\s*` + price)
	radius       = regexp.MustCompile(`(?i)(?:\ba\s+)?\b(\d+)\s*km\b`)
)

// FromText is a search written the way it is said, for the phone app, which has no way to
// share one: "bici 100-300", "kallax hasta 40", "moto a 30 km". What is not a price or a
// radius is the text searched for.
func FromText(text string) (url.Values, error) {
	query := url.Values{}
	take := func(re *regexp.Regexp, set func(m []string)) {
		if m := re.FindStringSubmatch(text); m != nil {
			set(m)
			text = strings.Replace(text, m[0], " ", 1)
		}
	}
	take(priceBetween, func(m []string) {
		query.Set("min_sale_price", plain(m[1]))
		query.Set("max_sale_price", plain(m[2]))
	})
	take(priceUpTo, func(m []string) { query.Set("max_sale_price", plain(m[1])) })
	take(priceFrom, func(m []string) { query.Set("min_sale_price", plain(m[1])) })
	take(radius, func(m []string) { query.Set("distance_in_km", m[1]) })

	keywords := strings.Join(strings.Fields(text), " ")
	if keywords == "" {
		return nil, ErrNoFilter
	}
	query.Set("keywords", keywords)
	return Searchable(query), nil
}

func plain(number string) string { return strings.ReplaceAll(number, ".", "") }

// Near limits a query to a radius around a point, keeping the radius it already asked for.
func Near(query url.Values, latitude, longitude float64, defaultKm int) url.Values {
	out := Searchable(query)
	out.Set("latitude", strconv.FormatFloat(latitude, 'f', 5, 64))
	out.Set("longitude", strconv.FormatFloat(longitude, 'f', 5, 64))
	if out.Get("distance_in_km") == "" {
		out.Set("distance_in_km", strconv.Itoa(defaultKm))
	}
	return out
}
