// Package reactivate reactivates the expired listings of the catalogue.
package reactivate

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
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

type Options struct {
	DryRun    bool
	MinPause  time.Duration
	MaxPause  time.Duration
	MaxPerRun int
}

type Failure struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Error string `json:"error"`
}

type Result struct {
	StartedAt   time.Time     `json:"started_at"`
	Duration    time.Duration `json:"duration"`
	DryRun      bool          `json:"dry_run,omitempty"`
	Catalogue   int           `json:"catalogue"`
	Expired     int           `json:"expired"`
	Skipped     int           `json:"skipped,omitempty"`
	Reactivated []string      `json:"reactivated,omitempty"`
	Failures    []Failure     `json:"failures,omitempty"`
	Error       string        `json:"error,omitempty"`
	// NeedsHuman: the session is gone, and retrying cannot fix it.
	NeedsHuman bool `json:"needs_human,omitempty"`
}

func (r Result) OK() bool { return r.Error == "" && len(r.Failures) == 0 }

func (r Result) Summary() string {
	if r.Error != "" {
		return "wallapop: la pasada ha fallado: " + r.Error
	}
	if r.Expired == 0 {
		return fmt.Sprintf("wallapop: %d anuncios, ninguno caducado", r.Catalogue)
	}
	verb := "reactivados"
	if r.DryRun {
		verb = "se reactivarian (dry run)"
	}
	msg := fmt.Sprintf("wallapop: %d de %d %s", len(r.Reactivated), r.Expired, verb)
	if len(r.Failures) > 0 {
		msg += fmt.Sprintf(", %d fallidos", len(r.Failures))
		for _, f := range r.Failures {
			msg += fmt.Sprintf("\n- %s: %s", f.Title, f.Error)
		}
	}
	return msg
}

func Run(ctx context.Context, client *wallapop.Client, opt Options, log *slog.Logger) Result {
	res := Result{StartedAt: time.Now(), DryRun: opt.DryRun}
	defer func() { res.Duration = time.Since(res.StartedAt).Round(time.Second) }()

	items, err := client.MyItems(ctx)
	if err != nil {
		res.Error = err.Error()
		res.NeedsHuman = errors.Is(err, wallapop.ErrUnauthorized)
		return res
	}
	res.Catalogue = len(items)

	var pending []wallapop.Item
	for _, item := range items {
		if item.NeedsReactivation() {
			pending = append(pending, item)
		}
	}
	res.Expired = len(pending)
	if len(pending) == 0 {
		log.Info("nothing to reactivate", "catalogue", res.Catalogue)
		return res
	}

	// Oldest first: they have been off the market longest.
	sort.Slice(pending, func(i, j int) bool { return pending[i].ModifiedDate < pending[j].ModifiedDate })
	if opt.MaxPerRun > 0 && len(pending) > opt.MaxPerRun {
		res.Skipped = len(pending) - opt.MaxPerRun
		pending = pending[:opt.MaxPerRun]
	}

	for i, item := range pending {
		if i > 0 && !opt.DryRun {
			if err := pause(ctx, opt.MinPause, opt.MaxPause); err != nil {
				res.Error = err.Error()
				return res
			}
		}

		if opt.DryRun {
			log.Info("would reactivate", "id", item.ID, "title", item.Title)
			res.Reactivated = append(res.Reactivated, item.Title)
			continue
		}

		if err := client.Reactivate(ctx, item.ID); err != nil {
			log.Error("reactivate failed", "id", item.ID, "title", item.Title, "err", err)
			res.Failures = append(res.Failures, Failure{ID: item.ID, Title: item.Title, Error: err.Error()})
			// A dead session fails every remaining item the same way, so stop here.
			if errors.Is(err, wallapop.ErrUnauthorized) {
				res.Error = err.Error()
				res.NeedsHuman = true
				return res
			}
			continue
		}
		log.Info("reactivated", "id", item.ID, "title", item.Title)
		res.Reactivated = append(res.Reactivated, item.Title)
	}
	return res
}

// Calls back to back are the pattern that reads as a bot.
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

func SaveResult(dir string, res Result) error {
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last_run.json"), raw, 0o644)
}

func LoadResult(dir string) (Result, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "last_run.json"))
	if err != nil {
		return Result{}, false
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return Result{}, false
	}
	return res, true
}
