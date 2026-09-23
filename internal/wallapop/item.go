package wallapop

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var PathItem = "/api/v3/items/"

const likeWords = 3

// Listing is what a search like it is made from.
type Listing struct {
	ID       string
	Title    string
	Category string
	Brand    string
	Price    float64
}

// ItemSlug is the listing an address points to: the app shares them as
// wallapop.com/item/<slug>, and the web as es.wallapop.com/item/<slug>.
func ItemSlug(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "wallapop.com" && !strings.HasSuffix(host, ".wallapop.com") {
		return "", false
	}
	slug, ok := strings.CutPrefix(strings.TrimSuffix(u.Path, "/"), "/item/")
	return slug, ok && slug != "" && !strings.Contains(slug, "/")
}

// The API answers a listing by its id, and an address only carries the slug: the id is in
// the page the slug opens.
var pageItemID = regexp.MustCompile(`"item":\{"id":"([a-z0-9]+)"`)

func (c *Client) Listing(ctx context.Context, slug string) (Listing, error) {
	id, err := c.itemID(ctx, slug)
	if err != nil {
		return Listing{}, err
	}
	var answer struct {
		ID    string `json:"id"`
		Title struct {
			Original string `json:"original"`
		} `json:"title"`
		Taxonomy []struct {
			ID string `json:"id"`
		} `json:"taxonomy"`
		Price struct {
			Cash struct {
				Amount float64 `json:"amount"`
			} `json:"cash"`
		} `json:"price"`
		TypeAttributes struct {
			Brand struct {
				Value string `json:"value"`
			} `json:"brand"`
		} `json:"type_attributes"`
	}
	if err := c.public(ctx, PathItem+id, nil, &answer); err != nil {
		return Listing{}, err
	}
	listing := Listing{ID: answer.ID, Title: strings.TrimSpace(answer.Title.Original),
		Brand: answer.TypeAttributes.Brand.Value, Price: answer.Price.Cash.Amount}
	if len(answer.Taxonomy) > 0 {
		listing.Category = answer.Taxonomy[0].ID
	}
	return listing, nil
}

func (c *Client) itemID(ctx context.Context, slug string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.WebURL+"/item/"+url.PathEscape(slug), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", "es,en-US;q=0.9")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("opening the listing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNoListing
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("opening the listing: the web answered %d", resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	m := pageItemID.FindSubmatch(page)
	if m == nil {
		return "", ErrNoListing
	}
	return string(m[1]), nil
}

var ErrNoListing = fmt.Errorf("ese anuncio ya no está en Wallapop")

// LikeListing is the search for things like a listing: the start of its title in its
// category, up to a fifth dearer than it. The whole title finds that listing alone, since
// every word has to be there.
func LikeListing(l Listing) url.Values {
	words := strings.Fields(l.Title)
	if len(words) > likeWords {
		words = words[:likeWords]
	}
	query := url.Values{"keywords": {strings.Join(words, " ")}}
	if l.Category != "" {
		query.Set("category_id", l.Category)
	}
	if l.Price > 0 {
		query.Set("max_sale_price", strconv.Itoa(int(math.Ceil(l.Price*1.2))))
	}
	return Searchable(query)
}
