package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

type Options struct {
	// Older listings are recorded in silence: they were already there.
	MaxAge        time.Duration
	MaxAlerts     int
	PhotosPerItem int
	// Pages is read on a deep or first round; any other round reads one page of 40.
	Pages   int
	Deep    bool
	SeenTTL time.Duration
	// Drop is a fraction of the lowest price; zero turns price drops off.
	Drop   float64
	DryRun bool
}

type Search struct {
	ID    string
	Name  string
	Query url.Values
}

// Notifier gets the whole search because the silence button needs its id.
type Notifier interface {
	Listing(ctx context.Context, search Search, item wallapop.SearchItem) error
	Cheaper(ctx context.Context, search Search, item wallapop.SearchItem, before float64) error
	Say(ctx context.Context, text string) error
}

type Failure struct {
	Search string `json:"search"`
	Error  string `json:"error"`
}

type Hit struct {
	Chat   string  `json:"chat,omitempty"`
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
	Users     int           `json:"users"`
	// Shared is how many searches were answered by another chat's identical request.
	Requests int  `json:"requests"`
	Shared   int  `json:"shared,omitempty"`
	Deep     bool `json:"deep,omitempty"`
	Watched  int  `json:"watched"`
	Silenced int  `json:"silenced,omitempty"`
	Scanned  int  `json:"scanned"`
	// Seeded is recorded without a message: a first pass, or too old to be news.
	Seeded     int       `json:"seeded"`
	Duplicates int       `json:"duplicates"`
	Cheaper    []Hit     `json:"cheaper,omitempty"`
	Held       int       `json:"held,omitempty"`
	New        []Hit     `json:"new,omitempty"`
	Failures   []Failure `json:"failures,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (r *Result) Merge(o Result) {
	r.Users++
	r.Watched += o.Watched
	r.Silenced += o.Silenced
	r.Scanned += o.Scanned
	r.Seeded += o.Seeded
	r.Duplicates += o.Duplicates
	r.Held += o.Held
	r.Cheaper = append(r.Cheaper, o.Cheaper...)
	r.New = append(r.New, o.New...)
	r.Failures = append(r.Failures, o.Failures...)
	if o.Error != "" && r.Error == "" {
		r.Error = o.Error
	}
}

func (r Result) OK() bool { return r.Error == "" && len(r.Failures) == 0 }

func (r Result) Summary() string {
	if r.Error != "" {
		return "wallapop: la ronda de busquedas ha fallado: " + r.Error
	}
	msg := fmt.Sprintf("wallapop: %d usuarios, %d busquedas, %d anuncios mirados, %d nuevos", r.Users, r.Watched, r.Scanned, len(r.New))
	if r.Requests > 0 {
		msg += fmt.Sprintf(", %d peticiones", r.Requests)
		if r.Shared > 0 {
			msg += fmt.Sprintf(" (%d compartidas)", r.Shared)
		}
	}
	if len(r.Cheaper) > 0 {
		msg += fmt.Sprintf(", %d mas baratos", len(r.Cheaper))
	}
	if r.Silenced > 0 {
		msg += fmt.Sprintf(", %d silenciadas", r.Silenced)
	}
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

// Run does one user's searches. Pacing the requests is the searcher's job.
func Run(ctx context.Context, client Searcher, seen *Seen, searches []Search, notify Notifier, opt Options, log *slog.Logger) Result {
	res := Result{StartedAt: time.Now(), DryRun: opt.DryRun, Deep: opt.Deep}
	defer func() { res.Duration = time.Since(res.StartedAt).Round(time.Second) }()

	hasher := NewHasher(opt.PhotosPerItem)
	now := time.Now()
	for _, search := range searches {
		res.Watched++
		if err := ctx.Err(); err != nil {
			res.Error = err.Error()
			break
		}

		firstPass := !seen.Watched(search.ID)
		pages := 1
		if opt.Deep || firstPass {
			pages = opt.Pages
		}
		items, err := client.Search(ctx, search.Query, pages)
		if err != nil {
			log.Error("search failed", "search", search.Name, "err", err)
			res.Failures = append(res.Failures, Failure{Search: search.Name, Error: err.Error()})
			continue
		}
		res.Scanned += len(items)

		// Oldest first, so Telegram gets them in the order they were posted.
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt < items[j].CreatedAt })

		for _, item := range items {
			if seen.Known(item.ID) {
				before, worth := seen.Cheaper(item, opt.Drop)
				if opt.Drop <= 0 || !worth {
					continue
				}
				if opt.MaxAlerts > 0 && len(res.New)+len(res.Cheaper) >= opt.MaxAlerts {
					res.Held++
					continue
				}
				res.Cheaper = append(res.Cheaper, Hit{
					Search: search.Name, Title: item.Title,
					Price: item.Price.Amount, City: item.Where(), URL: item.URL(),
				})
				log.Info("cheaper", "title", item.Title, "before", before, "now", item.Price.Amount)
				if notify == nil {
					continue
				}
				if err := notify.Cheaper(ctx, search, item, before); err != nil {
					log.Error("could not send the drop", "title", item.Title, "err", err)
					res.Failures = append(res.Failures, Failure{Search: search.Name, Error: err.Error()})
				}
				continue
			}

			hashes := hasher.Hashes(ctx, photoURLs(item))
			if rec, reason, dup := seen.Duplicate(item, hashes); dup {
				log.Info("duplicate", "title", item.Title, "of", rec.Title, "why", reason, "search", search.Name)
				res.Duplicates++
				// A copy of the announced one, so its price drops stay quiet too.
				owner := rec.ID
				if rec.CopyOf != "" {
					owner = rec.CopyOf
				}
				seen.AddCopy(item, hashes, search.Name, owner, now)
				continue
			}
			seen.Add(item, hashes, search.Name, now)

			switch {
			case firstPass, opt.MaxAge > 0 && now.Sub(item.Created()) > opt.MaxAge:
				res.Seeded++
			case opt.MaxAlerts > 0 && len(res.New)+len(res.Cheaper) >= opt.MaxAlerts:
				res.Held++
			default:
				res.New = append(res.New, Hit{
					Search: search.Name, Title: item.Title,
					Price: item.Price.Amount, City: item.Where(), URL: item.URL(),
				})
				if notify == nil {
					continue
				}
				if err := notify.Listing(ctx, search, item); err != nil {
					log.Error("could not send the message", "title", item.Title, "err", err)
					res.Failures = append(res.Failures, Failure{Search: search.Name, Error: err.Error()})
				}
			}
		}
		seen.MarkWatched(search.ID, now)
	}

	// A flood is capped: a hundred messages in a row is a reason to mute the bot.
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

// Pause is random so the requests do not go out like a metronome.
func Pause(ctx context.Context, min, max time.Duration) error {
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

// Line is built here so the dry run prints exactly what Telegram would get.
func Line(search string, item wallapop.SearchItem, escape func(string) string) string {
	if escape == nil {
		escape = func(s string) string { return s }
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🆕 <b>%s</b>\n", escape(item.Title))
	fmt.Fprintf(&b, "<b>%s</b>", Money(item.Price.Amount, item.Price.Currency))
	if where := item.Where(); where != "" {
		fmt.Fprintf(&b, " · %s", escape(where))
	}
	if item.Reserved != nil && item.Reserved.Flag {
		b.WriteString(" · <i>reservado</i>")
	}
	if search != "" {
		fmt.Fprintf(&b, "\n<i>🔎 %s</i>", escape(search))
	}
	return b.String()
}

func CheaperLine(search string, item wallapop.SearchItem, before float64, escape func(string) string) string {
	if escape == nil {
		escape = func(s string) string { return s }
	}
	now := item.Price.Amount
	var b strings.Builder
	fmt.Fprintf(&b, "📉 <b>%s</b>\n", escape(item.Title))
	fmt.Fprintf(&b, "<b>%s</b> · antes %s", Money(now, item.Price.Currency), Money(before, item.Price.Currency))
	if before > 0 && now < before {
		fmt.Fprintf(&b, " (−%.0f%%)", (before-now)/before*100)
	}
	if where := item.Where(); where != "" {
		fmt.Fprintf(&b, "\n%s", escape(where))
	}
	if search != "" {
		fmt.Fprintf(&b, "\n<i>🔎 %s</i>", escape(search))
	}
	return b.String()
}

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
