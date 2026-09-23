package watch

import (
	"context"
	"net/url"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

// Searcher is where a round reads a search from.
type Searcher interface {
	Search(ctx context.Context, query url.Values, pages int) ([]wallapop.SearchItem, error)
}

// Catalogue is one round's view of Wallapop: a query watched by several chats is asked
// once, and the requests that do go out are spaced by a random pause.
type Catalogue struct {
	client             Searcher
	minPause, maxPause time.Duration
	answers            map[string]answer
	// Requests is how many searches actually went out, and Shared how many were answered
	// from one that already had.
	Requests int
	Shared   int
}

type answer struct {
	items []wallapop.SearchItem
	pages int
	err   error
}

func NewCatalogue(client Searcher, minPause, maxPause time.Duration) *Catalogue {
	return &Catalogue{client: client, minPause: minPause, maxPause: maxPause, answers: map[string]answer{}}
}

// Search answers from this round's earlier request for the same query when it read at
// least as many pages, and asks Wallapop otherwise.
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
