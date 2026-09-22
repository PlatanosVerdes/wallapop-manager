package wallapop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// The saved searches are the ones kept in the app, so the phone stays the place where
// searches are added and removed. Replaying one is a plain search call: the stored query
// carries Wallapop's own parameter names.
var (
	PathSavedSearches = "/api/v3/searchalerts/savedsearch"
	PathSearch        = "/api/v3/search"
)

// ItemURL is where a listing is read by a human.
const ItemURL = "https://es.wallapop.com/item/"

type SavedSearch struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Query is the search as the app stored it, values already typed by the API.
	Query map[string]any `json:"query"`
	Alert struct {
		Enabled  bool `json:"enabled"`
		Distance int  `json:"distance"`
	} `json:"alert"`
	LocationLabel string `json:"location_label"`
	Description   string `json:"description"`
}

// Name is what a message calls the search. The title is what was typed in the app and can
// be empty on a search saved from a filtered category.
func (s SavedSearch) Name() string {
	if s.Title != "" {
		return s.Title
	}
	if s.Description != "" {
		return s.Description
	}
	return s.ID
}

// Values turns the stored query into a request. order_by is forced to newest because the
// point is what has just appeared, and saved_search_id is dropped: it is bookkeeping the
// search endpoint does not need.
func (s SavedSearch) Values() url.Values {
	query := url.Values{}
	for key, value := range s.Query {
		if key == "saved_search_id" || key == "order_by" {
			continue
		}
		if encoded := format(value); encoded != "" {
			query.Set(key, encoded)
		}
	}
	query.Set("order_by", "newest")
	query.Set("source", "saved_search")
	if s.Alert.Distance > 0 && query.Get("distance") == "" {
		query.Set("distance", strconv.Itoa(s.Alert.Distance))
	}
	return query
}

// format writes a JSON value the way a query string carries it: whole numbers without the
// decimal point JSON decoding gives them.
func format(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// SavedSearches reads the searches kept in the app. This is the one watcher call that
// needs the session.
func (c *Client) SavedSearches(ctx context.Context) ([]SavedSearch, error) {
	var raw json.RawMessage
	if _, err := c.do(ctx, "GET", PathSavedSearches, nil, nil, &raw); err != nil {
		return nil, err
	}
	return decodeSearches(raw)
}

type Image struct {
	URLs struct {
		Small  string `json:"small"`
		Medium string `json:"medium"`
		Big    string `json:"big"`
	} `json:"urls"`
}

type Location struct {
	City        string `json:"city"`
	PostalCode  string `json:"postal_code"`
	Region      string `json:"region"`
	CountryCode string `json:"country_code"`
}

type SearchItem struct {
	ID          string   `json:"id"`
	UserID      string   `json:"user_id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Price       Price    `json:"price"`
	WebSlug     string   `json:"web_slug"`
	CreatedAt   int64    `json:"created_at"`
	ModifiedAt  int64    `json:"modified_at"`
	Images      []Image  `json:"images"`
	Location    Location `json:"location"`
	Reserved    *Flag    `json:"reserved"`
}

func (i SearchItem) Created() time.Time {
	if i.CreatedAt == 0 {
		return time.Time{}
	}
	return time.UnixMilli(i.CreatedAt)
}

func (i SearchItem) URL() string {
	if i.WebSlug == "" {
		return ItemURL + i.ID
	}
	return ItemURL + i.WebSlug
}

func (i SearchItem) Photo() string {
	if len(i.Images) == 0 {
		return ""
	}
	if url := i.Images[0].URLs.Big; url != "" {
		return url
	}
	return i.Images[0].URLs.Medium
}

// Where is the town a listing sits in, which is the field that tells two copies of the
// same vehicle apart.
func (i SearchItem) Where() string {
	if i.Location.City != "" {
		return i.Location.City
	}
	return i.Location.Region
}

// Search runs a query against the public catalogue, following the cursor for up to pages
// pages. It carries no session: the endpoint answers the same to anybody, and an anonymous
// call cannot get the account flagged.
//
// A page is 40 listings and a saved search can hold more, so a single page would leave the
// tail of it unwatched: those listings would never be seen to change price.
func (c *Client) Search(ctx context.Context, query url.Values, pages int) ([]SearchItem, error) {
	if pages < 1 {
		pages = 1
	}

	var all []SearchItem
	next := ""
	for page := 0; page < pages; page++ {
		asked := url.Values{}
		for key, values := range query {
			asked[key] = values
		}
		if next != "" {
			asked.Set("next_page", next)
		}

		var answer struct {
			Data struct {
				Section struct {
					Payload struct {
						Items []SearchItem `json:"items"`
					} `json:"payload"`
				} `json:"section"`
			} `json:"data"`
			Meta struct {
				NextPage string `json:"next_page"`
			} `json:"meta"`
		}
		if err := c.public(ctx, PathSearch, asked, &answer); err != nil {
			return all, err
		}

		items := answer.Data.Section.Payload.Items
		all = append(all, items...)
		next = answer.Meta.NextPage
		if next == "" || len(items) == 0 {
			break
		}
	}
	return all, nil
}

// decodeSearches takes the bare array the endpoint answers today, and the {"data": [...]}
// wrapper the rest of the API uses, in case this one ever joins them.
func decodeSearches(raw json.RawMessage) ([]SavedSearch, error) {
	var bare []SavedSearch
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare, nil
	}
	var wrapped struct {
		Data []SavedSearch `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		return wrapped.Data, nil
	}
	return nil, fmt.Errorf("wallapop: the saved searches response was not a list: %s", snippet(raw))
}
