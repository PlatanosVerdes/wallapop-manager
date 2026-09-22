package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

type Options struct {
	// All watches every saved search. Off, only the ones whose alert is on in the app are
	// followed, which is how the switch on the phone keeps deciding.
	All bool
	// MaxAge is how recent a listing has to be to be worth a message. Anything older is
	// recorded in silence: it was already there, the search just reached it.
	MaxAge    time.Duration
	MaxAlerts int
	// PhotosPerItem is how many pictures of a new listing are hashed.
	PhotosPerItem      int
	SeenTTL            time.Duration
	MinPause, MaxPause time.Duration
	DryRun             bool
}

// Notifier is what says a listing out loud.
type Notifier interface {
	Listing(ctx context.Context, search string, item wallapop.SearchItem) error
	Say(ctx context.Context, text string) error
}

type Failure struct {
	Search string `json:"search"`
	Error  string `json:"error"`
}

type Hit struct {
	Search string  `json:"search"`
	Title  string  `json:"title"`
	Price  float64 `json:"price"`
	City   string  `json:"city,omitempty"`
	URL    string  `json:"url"`
}

type Result struct {
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration"`
	DryRun    bool          `json:"dry_run,omitempty"`
	// Watched and Ignored split the saved searches by the alert switch in the app.
	Watched int `json:"watched"`
	Ignored int `json:"ignored"`
	Scanned int `json:"scanned"`
	// Seeded is what was recorded without a message: the first pass of a search, and
	// listings already too old to be news.
	Seeded     int       `json:"seeded"`
	Duplicates int       `json:"duplicates"`
	Held       int       `json:"held,omitempty"`
	New        []Hit     `json:"new,omitempty"`
	Failures   []Failure `json:"failures,omitempty"`
	Error      string    `json:"error,omitempty"`
	NeedsHuman bool      `json:"needs_human,omitempty"`
}

func (r Result) OK() bool { return r.Error == "" && len(r.Failures) == 0 }

func (r Result) Summary() string {
	if r.Error != "" {
		return "wallapop: la ronda de busquedas ha fallado: " + r.Error
	}
	msg := fmt.Sprintf("wallapop: %d busquedas, %d anuncios mirados, %d nuevos", r.Watched, r.Scanned, len(r.New))
	if r.Duplicates > 0 {
		msg += fmt.Sprintf(", %d repetidos descartados", r.Duplicates)
	}
	if r.Seeded > 0 {
		msg += fmt.Sprintf(", %d apuntados sin avisar", r.Seeded)
	}
	for _, hit := range r.New {
		msg += fmt.Sprintf("\n- [%s] %s, %.0f EUR, %s", hit.Search, hit.Title, hit.Price, hit.City)
	}
	for _, f := range r.Failures {
		msg += fmt.Sprintf("\n- %s: %s", f.Search, f.Error)
	}
	return msg
}

// Run reads the saved searches and announces what is genuinely new in them.
func Run(ctx context.Context, client *wallapop.Client, seen *Seen, notify Notifier, opt Options, log *slog.Logger) Result {
	res := Result{StartedAt: time.Now(), DryRun: opt.DryRun}
	defer func() { res.Duration = time.Since(res.StartedAt).Round(time.Second) }()

	searches, err := client.SavedSearches(ctx)
	if err != nil {
		res.Error = err.Error()
		res.NeedsHuman = errors.Is(err, wallapop.ErrUnauthorized)
		return res
	}

	hasher := NewHasher(opt.PhotosPerItem)
	now := time.Now()
	for _, search := range searches {
		// The switch in the app is the setting. A search with its alert off is one he
		// turned off, and this must not quietly turn it back on.
		if !search.Alert.Enabled && !opt.All {
			res.Ignored++
			continue
		}
		res.Watched++

		if res.Watched > 1 {
			if err := pause(ctx, opt.MinPause, opt.MaxPause); err != nil {
				res.Error = err.Error()
				break
			}
		}

		items, err := client.Search(ctx, search.Values())
		if err != nil {
			log.Error("search failed", "search", search.Name(), "err", err)
			res.Failures = append(res.Failures, Failure{Search: search.Name(), Error: err.Error()})
			continue
		}
		res.Scanned += len(items)

		// Oldest first, so several listings arriving together reach Telegram in the order
		// they were posted.
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt < items[j].CreatedAt })
		firstPass := !seen.Watched(search.ID)

		for _, item := range items {
			if seen.Known(item.ID) {
				continue
			}

			hashes := hasher.Hashes(ctx, photoURLs(item))
			if rec, reason, dup := seen.Duplicate(item, hashes); dup {
				log.Info("duplicate", "title", item.Title, "of", rec.Title, "why", reason, "search", search.Name())
				res.Duplicates++
				seen.Add(item, hashes, search.Name(), now)
				continue
			}
			seen.Add(item, hashes, search.Name(), now)

			switch {
			case firstPass, opt.MaxAge > 0 && now.Sub(item.Created()) > opt.MaxAge:
				res.Seeded++
			case opt.MaxAlerts > 0 && len(res.New) >= opt.MaxAlerts:
				res.Held++
			default:
				res.New = append(res.New, Hit{
					Search: search.Name(), Title: item.Title,
					Price: item.Price.Amount, City: item.Where(), URL: item.URL(),
				})
				if notify == nil {
					continue
				}
				if err := notify.Listing(ctx, search.Name(), item); err != nil {
					log.Error("could not send the message", "title", item.Title, "err", err)
					res.Failures = append(res.Failures, Failure{Search: search.Name(), Error: err.Error()})
				}
			}
		}
		seen.MarkWatched(search.ID, now)
	}

	// A flood is capped rather than sent: a hundred messages in a row is not an alert, it
	// is a reason to mute the bot.
	if res.Held > 0 && notify != nil && !opt.DryRun {
		text := fmt.Sprintf("… y %d anuncios nuevos mas en esta ronda, sin mandar.", res.Held)
		if err := notify.Say(ctx, text); err != nil {
			log.Error("could not send the tail message", "err", err)
		}
	}

	if opt.DryRun {
		return res
	}
	seen.Prune(opt.SeenTTL, now)
	if err := seen.Save(); err != nil {
		log.Error("could not save what has been seen", "err", err)
		res.Failures = append(res.Failures, Failure{Search: "seen.json", Error: err.Error()})
	}
	return res
}

func photoURLs(item wallapop.SearchItem) []string {
	var urls []string
	for _, img := range item.Images {
		if img.URLs.Small != "" {
			urls = append(urls, img.URLs.Small)
		}
	}
	return urls
}

func pause(ctx context.Context, min, max time.Duration) error {
	wait := min
	if max > min {
		wait += rand.N(max - min)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// Interval is the wait until the next round: never the same twice, which is the point.
func Interval(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + rand.N(max-min)
}

func SaveResult(dir string, res Result) error {
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last_watch.json"), raw, 0o644)
}

func LoadResult(dir string) (Result, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "last_watch.json"))
	if err != nil {
		return Result{}, false
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return Result{}, false
	}
	return res, true
}

// Line is the message a listing gets. It is built here so the dry run prints exactly what
// Telegram would receive.
func Line(search string, item wallapop.SearchItem, escape func(string) string) string {
	if escape == nil {
		escape = func(s string) string { return s }
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🆕 <b>%s</b>\n", escape(item.Title))
	fmt.Fprintf(&b, "%s", Money(item.Price.Amount, item.Price.Currency))
	if where := item.Where(); where != "" {
		fmt.Fprintf(&b, " · %s", escape(where))
	}
	if item.Reserved != nil && item.Reserved.Flag {
		b.WriteString(" · reservado")
	}
	// The bot is shared with the other small services, so the message says who is talking.
	if search != "" {
		fmt.Fprintf(&b, "\n🔎 wallapop · %s", escape(search))
	}
	fmt.Fprintf(&b, "\n%s", item.URL())
	return b.String()
}

// Money writes a price the way it is read here: thousands separated by a dot, no decimals
// when there are none.
func Money(amount float64, currency string) string {
	symbol := "€"
	if currency != "" && currency != "EUR" {
		symbol = currency
	}

	whole := fmt.Sprintf("%.0f", amount)
	var parts []string
	for len(whole) > 3 {
		parts = append([]string{whole[len(whole)-3:]}, parts...)
		whole = whole[:len(whole)-3]
	}
	parts = append([]string{whole}, parts...)
	return strings.Join(parts, ".") + " " + symbol
}
