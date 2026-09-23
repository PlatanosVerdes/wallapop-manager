package wallapop

import (
	"context"
	"net/url"
	"time"
)

var PathSearch = "/api/v3/search"

// ItemURL is where a listing is read by a human.
const ItemURL = "https://es.wallapop.com/item/"

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
// A page is 40 listings and a search can hold more, so a single page would leave the
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
