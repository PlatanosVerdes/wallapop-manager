package watch

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

type Searcher interface {
	Search(ctx context.Context, query url.Values, pages int) ([]wallapop.SearchItem, error)
}

type Detailer interface {
	Details(ctx context.Context, id string) (wallapop.Details, error)
}

// Catalogue asks each query once per round, however many chats watch it.
type Catalogue struct {
	client             Searcher
	minPause, maxPause time.Duration
	answers            map[string]answer
	details            map[string]wallapop.Details
	Requests           int
	Shared             int
}

type answer struct {
	items []wallapop.SearchItem
	pages int
	err   error
}

func NewCatalogue(client Searcher, minPause, maxPause time.Duration) *Catalogue {
	return &Catalogue{client: client, minPause: minPause, maxPause: maxPause, answers: map[string]answer{}, details: map[string]wallapop.Details{}}
}

func (c *Catalogue) Search(ctx context.Context, query url.Values, pages int) ([]wallapop.SearchItem, error) {
	key := query.Encode()
	if a, ok := c.answers[key]; ok && (a.err != nil || a.pages >= pages) {
		c.Shared++
		return append([]wallapop.SearchItem(nil), a.items...), a.err
	}
	if c.Requests > 0 {
		if err := Pause(ctx, c.minPause, c.maxPause); err != nil {
			return nil, err
		}
	}
	c.Requests++
	items, err := c.client.Search(ctx, query, pages)
	c.answers[key] = answer{items: items, pages: pages, err: err}
	return append([]wallapop.SearchItem(nil), items...), err
}

// Details asks each listing once per round, paced like a search, and only when the client
// can: a listing found by two chats costs one request.
func (c *Catalogue) Details(ctx context.Context, id string) (wallapop.Details, error) {
	if d, ok := c.details[id]; ok {
		return d, nil
	}
	detailer, ok := c.client.(Detailer)
	if !ok {
		return wallapop.Details{}, errNoDetails
	}
	if err := Pause(ctx, c.minPause, c.maxPause); err != nil {
		return wallapop.Details{}, err
	}
	d, err := detailer.Details(ctx, id)
	if err == nil {
		c.details[id] = d
	}
	return d, err
}

var errNoDetails = errors.New("this client cannot read listing details")
