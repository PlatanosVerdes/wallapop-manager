package wallapop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Taken from a captured browser request; variables so a change on their side is a redeploy.
var (
	PathItems        = "/api/v3/user/items"
	PathReactivate   = "/api/v3/items/%s/reactivate"
	ReactivateMethod = "PUT"
	ItemsPageSize    = 100
)

var ErrNoItemsDecoded = errors.New("wallapop: the items response carried no recognisable list")

// HeaderNextPage carries the cursor; the body's meta object comes back empty.
const HeaderNextPage = "X-NextPage"

type Price struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Flag is the API's yes/no: {"flag": true}, or absent for no.
type Flag struct {
	Flag bool `json:"flag"`
}

type Item struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CategoryID   string `json:"category_id"`
	Slug         string `json:"slug"`
	CreatedDate  int64  `json:"created_date"`
	ModifiedDate int64  `json:"modified_date"`
	Price        Price  `json:"price"`
	Expired      *Flag  `json:"expired"`
}

func (i Item) Modified() time.Time {
	if i.ModifiedDate == 0 {
		return time.Time{}
	}
	return time.UnixMilli(i.ModifiedDate)
}

func (i Item) NeedsReactivation() bool { return i.Expired != nil && i.Expired.Flag }

func (i Item) String() string {
	return fmt.Sprintf("%s (%.0f %s)", i.Title, i.Price.Amount, i.Price.Currency)
}

func (c *Client) MyItems(ctx context.Context) ([]Item, error) {
	var all []Item
	next := ""
	for page := 0; page < 50; page++ {
		query := url.Values{}
		query.Set("limit", strconv.Itoa(ItemsPageSize))
		if next != "" {
			query.Set("next_page", next)
		}

		var raw json.RawMessage
		header, err := c.do(ctx, "GET", PathItems, query, nil, &raw)
		if err != nil {
			return nil, err
		}
		items, err := decodeItems(raw)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)

		next = header.Get(HeaderNextPage)
		if next == "" || len(items) == 0 {
			break
		}
	}
	return all, nil
}

func decodeItems(raw json.RawMessage) ([]Item, error) {
	var wrapped struct {
		Data []Item `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Data != nil {
		return wrapped.Data, nil
	}
	var bare []Item
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNoItemsDecoded, snippet(raw))
}

// Reactivate succeeds with a 204 and no body.
func (c *Client) Reactivate(ctx context.Context, itemID string) error {
	_, err := c.do(ctx, ReactivateMethod, fmt.Sprintf(PathReactivate, itemID), nil, nil, nil)
	return err
}
