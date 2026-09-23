package watch

import (
	"context"
	"net/url"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

type Searcher interface {
	Search(ctx context.Context, query url.Values, pages int) ([]wallapop.SearchItem, error)
}

// Catalogue asks each query once per round, however many chats watch it.
type Catalogue struct {
	client             Searcher
	minPause, maxPause time.Duration
	answers            map[string]answer
	Requests           int
	Shared             int
}

type answer struct {
	items []wallapop.SearchItem
	pages int
	err   error
}

func NewCatalogue(client Searcher, minPause, maxPause time.Duration) *Catalogue {
	return &Catalogue{client: client, minPause: minPause, maxPause: maxPause, answers: map[string]answer{}}
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
