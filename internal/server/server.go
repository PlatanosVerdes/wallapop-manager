// Package server exposes the health endpoint. It answers one question: does this need a
// human? A dead session does; a failed pass that will retry does not.
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
	// The access token lasts minutes and is minted on every pass, so what is worth
	// reporting is how long the session can keep minting them.
	RenewableDays *float64           `json:"renewable_days_left,omitempty"`
	NextRun       string             `json:"next_run,omitempty"`
	LastRun       *reactivate.Result `json:"last_run,omitempty"`
	NextWatch     string             `json:"next_watch,omitempty"`
	LastWatch     *watch.Result      `json:"last_watch,omitempty"`
}

func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Always 200 while the process is alive. A session that needs a human is not a
		// service that stopped answering, and the blackbox probe reads any other code as
		// exactly that: the alert for the session comes from wallapop_last_run_status.
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
				// Only when the session itself looks fine: otherwise its own reason is
				// the specific one and this would bury it.
				down("the last pass needs a human: " + res.Error)
			case res.NeedsHuman:
				body.Status = "down"
			case res.Error != "" && body.Status == "ok":
				body.Status = "warn"
			}
		}
		// The watcher is reported but does not decide the status: a search that failed is
		// tried again in minutes and is nobody's emergency.
		if res, ok := watch.LoadResult(h.DataDir); ok {
			body.LastWatch = &res
			if res.NeedsHuman && body.Status == "ok" {
				down("the last round of searches needs a human: " + res.Error)
			}
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
