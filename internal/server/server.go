// Package server answers /healthz: a dead session needs a human, a failed pass does not.
package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/reactivate"
	"github.com/PlatanosVerdes/wallapop-manager/internal/session"
	"github.com/PlatanosVerdes/wallapop-manager/internal/watch"
)

type Health struct {
	Version    string
	DataDir    string
	Store      *session.Store
	WarnBefore time.Duration
	NextRun    func() time.Time
	NextWatch  func() time.Time
}

type payload struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Session string `json:"session"`
	// The access token lasts minutes; what matters is how long the session can mint them.
	RenewableDays *float64           `json:"renewable_days_left,omitempty"`
	NextRun       string             `json:"next_run,omitempty"`
	LastRun       *reactivate.Result `json:"last_run,omitempty"`
	NextWatch     string             `json:"next_watch,omitempty"`
	LastWatch     *watch.Result      `json:"last_watch,omitempty"`
}

func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Always 200: the blackbox probe reads anything else as down. The session alert
		// comes from wallapop_last_run_status.
		body := payload{Status: "ok", Version: h.Version, Session: "ok"}
		down := func(why string) {
			body.Session = why
			body.Status = "down"
		}

		sess, err := h.Store.Load()
		switch {
		case err != nil:
			down(err.Error())
		case sess.CookieValue == "":
			down("no session cookie stored")
		default:
			if left, ok := sess.Renewable(); ok {
				days := left.Hours() / 24
				body.RenewableDays = &days
				switch {
				case left <= 0:
					down("the session cookie has expired")
				case left <= h.WarnBefore:
					body.Session = "expiring"
					body.Status = "warn"
				}
			}
		}

		if res, ok := reactivate.LoadResult(h.DataDir); ok {
			body.LastRun = &res
			switch {
			case res.NeedsHuman && body.Status == "ok":
				// Only when the session looks fine, so its own reason is not buried.
				down("the last pass needs a human: " + res.Error)
			case res.NeedsHuman:
				body.Status = "down"
			case res.Error != "" && body.Status == "ok":
				body.Status = "warn"
			}
		}
		// A failed search retries in minutes, so it does not decide the status.
		if res, ok := watch.LoadResult(h.DataDir); ok {
			body.LastWatch = &res
		}
		if h.NextRun != nil {
			if next := h.NextRun(); !next.IsZero() {
				body.NextRun = next.Format(time.RFC3339)
			}
		}
		if h.NextWatch != nil {
			if next := h.NextWatch(); !next.IsZero() {
				body.NextWatch = next.Format(time.RFC3339)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(body)
	})
	return mux
}
