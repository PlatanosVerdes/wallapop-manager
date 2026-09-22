package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/config"
	"github.com/PlatanosVerdes/wallapop-manager/internal/metrics"
	"github.com/PlatanosVerdes/wallapop-manager/internal/reactivate"
	"github.com/PlatanosVerdes/wallapop-manager/internal/server"
	"github.com/PlatanosVerdes/wallapop-manager/internal/session"
	"github.com/PlatanosVerdes/wallapop-manager/internal/telegram"
	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
	"github.com/PlatanosVerdes/wallapop-manager/internal/watch"
)

var buildVersion = "local"

const usage = `wallapop-manager ` + "%s" + `

  run [--dry-run]        one pass: reactivate everything expired
  serve [--port] [--interval]
                         daily pass plus /healthz
  session import --cookie <value>
                         store the browser session cookie (also reads it on stdin)
  session show           what is stored and whether it can renew itself
  session refresh        renew now, which is how you check the session works
  sign <method> <path> <timestamp> [signature]
                         reproduce an X-Signature, or say which scheme matches a captured one

Configured through the environment (WALLA_*); see the README.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Printf(usage, buildVersion)
		return errors.New("no command given")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	log := newLogger(cfg.LogJSON)
	store := session.NewStore(cfg.DataDir)

	switch args[0] {
	case "run":
		return cmdRun(cfg, store, log, args[1:])
	case "watch":
		return cmdWatch(cfg, store, log, args[1:])
	case "searches":
		return cmdSearches(cfg, store)
	case "serve":
		return cmdServe(cfg, store, log, args[1:])
	case "session":
		return cmdSession(cfg, store, args[1:])
	case "sign":
		return cmdSign(cfg, args[1:])
	case "help", "-h", "--help":
		fmt.Printf(usage, buildVersion)
		return nil
	default:
		fmt.Printf(usage, buildVersion)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func newLogger(asJSON bool) *slog.Logger {
	if asJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

func newClient(cfg config.Config, store *session.Store) *wallapop.Client {
	client := wallapop.New(store)
	client.BaseURL = cfg.BaseURL
	client.WebURL = cfg.WebURL
	client.Scheme = cfg.Scheme
	client.AppVersion = cfg.AppVersion
	client.DeviceID = cfg.DeviceID
	if client.DeviceID == "" {
		if sess := store.Current(); sess != nil {
			client.DeviceID = sess.Device()
		}
	}
	return client
}

func cmdRun(cfg config.Config, store *session.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "list what would be reactivated without touching anything")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	res := onePass(ctx, cfg, store, log, *dryRun)
	fmt.Println(res.Summary())
	if !res.OK() {
		return errors.New("the pass did not finish clean")
	}
	return nil
}

func cmdServe(cfg config.Config, store *session.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", cfg.Port, "port for /healthz")
	interval := fs.Duration("interval", cfg.Interval, "time between reactivation passes")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Written by the loops and read by the health handler, so they cross goroutines.
	var next, nextWatch atomic.Int64
	next.Store(time.Now().Unix())
	nextWatch.Store(time.Now().Unix())
	health := &server.Health{
		Version:    buildVersion,
		DataDir:    cfg.DataDir,
		Store:      store,
		WarnBefore: cfg.WarnBefore,
		NextRun:    func() time.Time { return time.Unix(next.Load(), 0) },
		NextWatch:  func() time.Time { return time.Unix(nextWatch.Load(), 0) },
	}
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(*port),
		Handler:           health.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", srv.Addr, "version", buildVersion)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server stopped", "err", err)
			stop()
		}
	}()

	// The two jobs keep their own clocks: the catalogue is a daily errand, the searches
	// are checked on a short random one so the pattern is not a metronome.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			res := onePass(ctx, cfg, store, log, false)

			// A pass that failed is retried on a short clock: a session imported by hand
			// should take effect in minutes, not on tomorrow's tick.
			wait := *interval
			if !res.OK() {
				wait = cfg.RetryEvery
			}
			at := time.Now().Add(wait)
			next.Store(at.Unix())
			log.Info("next pass", "at", at.Format(time.RFC3339), "after_failure", !res.OK())
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			res := watchPass(ctx, cfg, store, log, watch.Options{All: cfg.WatchAll})

			wait := watch.Interval(cfg.WatchMin, cfg.WatchMax)
			if !res.OK() {
				wait = maxDuration(wait, cfg.RetryEvery)
			}
			at := time.Now().Add(wait)
			nextWatch.Store(at.Unix())
			log.Info("next watch", "at", at.Format(time.RFC3339), "new", len(res.New))
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()

	wg.Wait()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func cmdWatch(cfg config.Config, store *session.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "say what would be announced without sending anything")
	all := fs.Bool("all", cfg.WatchAll, "watch every saved search, not only the ones with the alert on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	res := watchPass(ctx, cfg, store, log, watch.Options{DryRun: *dryRun, All: *all})
	fmt.Println(res.Summary())
	if !res.OK() {
		return errors.New("the round did not finish clean")
	}
	return nil
}

func cmdSearches(cfg config.Config, store *session.Store) error {
	if _, err := store.Load(); err != nil {
		return err
	}
	searches, err := newClient(cfg, store).SavedSearches(context.Background())
	if err != nil {
		return err
	}
	if len(searches) == 0 {
		fmt.Println("no saved searches in the account")
		return nil
	}

	for _, search := range searches {
		mark := "off"
		if search.Alert.Enabled {
			mark = "ON "
		}
		fmt.Printf("%s  %-28s %s\n", mark, search.Name(), search.LocationLabel)
		fmt.Printf("     %s\n", search.Values().Encode())
	}
	fmt.Println()
	fmt.Println("Only the searches marked ON are watched; the switch is the one in the app.")
	if cfg.WatchAll {
		fmt.Println("WALLA_WATCH_ALL is set, so every search above is watched anyway.")
	}
	return nil
}

// watchPass reads the saved searches and announces what is new in them. Telegram carries
// only this: the listings asked for. Everything else the service knows is a metric.
func watchPass(ctx context.Context, cfg config.Config, store *session.Store, log *slog.Logger, opt watch.Options) watch.Result {
	opt.MaxAge = cfg.WatchMaxAge
	opt.MaxAlerts = cfg.WatchMaxAlerts
	opt.PhotosPerItem = cfg.WatchPhotos
	opt.SeenTTL = cfg.SeenTTL
	opt.MinPause = cfg.WatchMinPause
	opt.MaxPause = cfg.WatchMaxPause

	report := func(res watch.Result) watch.Result {
		if opt.DryRun {
			return res
		}
		if err := watch.SaveResult(cfg.DataDir, res); err != nil {
			log.Error("could not save the round", "err", err)
		}
		if err := pushWatch(ctx, cfg, res); err != nil {
			log.Error("could not push watch metrics", "err", err)
		}
		return res
	}

	if _, err := store.Load(); err != nil {
		log.Error("no usable session", "err", err)
		return report(watch.Result{StartedAt: time.Now(), Error: err.Error(), NeedsHuman: true})
	}
	seen, err := watch.LoadSeen(cfg.DataDir)
	if err != nil {
		log.Error("could not read what has been seen", "err", err)
		return report(watch.Result{StartedAt: time.Now(), Error: err.Error()})
	}

	client := newClient(cfg, store)
	if store.AccessSpent() {
		if err := client.RenewSession(ctx); err != nil {
			log.Error("the session could not be renewed", "err", err)
			return report(watch.Result{StartedAt: time.Now(), Error: err.Error(), NeedsHuman: true})
		}
	}

	var notify watch.Notifier
	switch bot := telegram.New(cfg.TelegramToken, cfg.TelegramChat); {
	case opt.DryRun:
		// The dry run prints the message it would have sent, markup and all.
		notify = &printer{}
	case bot.Enabled():
		notify = &messenger{bot: bot}
	default:
		log.Warn("no telegram configured, so nothing will be announced")
	}
	return report(watch.Run(ctx, client, seen, notify, opt, log))
}

// messenger turns a listing into the message that reaches the phone.
type messenger struct{ bot *telegram.Bot }

func (m *messenger) Listing(ctx context.Context, search string, item wallapop.SearchItem) error {
	return m.bot.Photo(ctx, item.Photo(), watch.Line(search, item, telegram.Escape))
}

func (m *messenger) Say(ctx context.Context, text string) error {
	return m.bot.Text(ctx, telegram.Escape(text))
}

type printer struct{}

func (printer) Listing(_ context.Context, search string, item wallapop.SearchItem) error {
	fmt.Println("---")
	fmt.Println(watch.Line(search, item, telegram.Escape))
	fmt.Println("foto:", item.Photo())
	return nil
}

func (printer) Say(_ context.Context, text string) error {
	fmt.Println("---")
	fmt.Println(text)
	return nil
}

// onePass renews the session, does the round, and reports state. It never sends a message
// of its own: the alert rules decide what is worth waking somebody for.
func onePass(ctx context.Context, cfg config.Config, store *session.Store, log *slog.Logger, dryRun bool) reactivate.Result {
	report := func(res reactivate.Result) reactivate.Result {
		if err := reactivate.SaveResult(cfg.DataDir, res); err != nil {
			log.Error("could not save the run", "err", err)
		}
		if dryRun {
			return res
		}
		if err := push(ctx, cfg, store, res); err != nil {
			log.Error("could not push metrics", "err", err)
		}
		return res
	}

	if _, err := store.Load(); err != nil {
		log.Error("no usable session", "err", err)
		return report(reactivate.Result{StartedAt: time.Now(), Error: err.Error(), NeedsHuman: true})
	}
	client := newClient(cfg, store)

	// The access token lasts five minutes, so it is spent between passes. Renewing up
	// front saves a rejected request; the client also renews on its own if a call is
	// rejected mid-pass.
	if store.AccessSpent() {
		if err := client.RenewSession(ctx); err != nil {
			log.Error("the session could not be renewed", "err", err)
			return report(reactivate.Result{StartedAt: time.Now(), Error: err.Error(), NeedsHuman: true})
		}
		log.Info("session renewed")
	}

	return report(reactivate.Run(ctx, client, reactivate.Options{
		DryRun:    dryRun,
		MinPause:  cfg.MinPause,
		MaxPause:  cfg.MaxPause,
		MaxPerRun: cfg.MaxPerRun,
	}, log))
}

// push reports the pass as gauges. Status follows the convention the other rules use:
// 0 is fine, 1 is a failure a retry may fix, 2 needs a human.
func push(ctx context.Context, cfg config.Config, store *session.Store, res reactivate.Result) error {
	status := 0.0
	switch {
	case res.NeedsHuman:
		status = 2
	case !res.OK():
		status = 1
	}

	gauges := []metrics.Gauge{
		{Name: "wallapop_last_run_status", Help: "0 fine, 1 failed, 2 needs a human", Value: status},
		{Name: "wallapop_last_run_timestamp", Help: "Unix time of the last pass", Value: float64(time.Now().Unix())},
		{Name: "wallapop_expired_listings", Help: "Listings found expired in the last pass", Value: float64(res.Expired)},
		{Name: "wallapop_reactivated_listings", Help: "Listings reactivated in the last pass", Value: float64(len(res.Reactivated))},
	}
	// Days of unattended runway left, and -1 when it is not known yet.
	days := -1.0
	if sess := store.Current(); sess != nil {
		if left, ok := sess.Renewable(); ok {
			days = left.Hours() / 24
		}
	}
	gauges = append(gauges, metrics.Gauge{
		Name:  "wallapop_session_days_remaining",
		Help:  "Days before the session has to be imported again by hand",
		Value: days,
	})

	return metrics.New(cfg.Pushgateway, "wallapop-manager").Push(ctx, gauges)
}

// pushWatch reports the round the same way: gauges, and the alert rules decide. The
// listings themselves are not metrics, only how many there were.
func pushWatch(ctx context.Context, cfg config.Config, res watch.Result) error {
	status := 0.0
	switch {
	case res.NeedsHuman:
		status = 2
	case !res.OK():
		status = 1
	}

	return metrics.New(cfg.Pushgateway, "wallapop-watch").Push(ctx, []metrics.Gauge{
		{Name: "wallapop_watch_status", Help: "0 fine, 1 failed, 2 needs a human", Value: status},
		{Name: "wallapop_watch_timestamp", Help: "Unix time of the last round of searches", Value: float64(time.Now().Unix())},
		{Name: "wallapop_watch_searches", Help: "Saved searches watched in the last round", Value: float64(res.Watched)},
		{Name: "wallapop_watch_scanned", Help: "Listings read in the last round", Value: float64(res.Scanned)},
		{Name: "wallapop_watch_new", Help: "Listings announced in the last round", Value: float64(len(res.New))},
		{Name: "wallapop_watch_duplicates", Help: "Listings dropped as a copy of one already seen", Value: float64(res.Duplicates)},
	})
}

func cmdSession(cfg config.Config, store *session.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("session needs a subcommand: import, show or refresh")
	}

	switch args[0] {
	case "import":
		fs := flag.NewFlagSet("session import", flag.ExitOnError)
		cookie := fs.String("cookie", "", "the "+session.DefaultCookieName+" cookie from the browser")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		var sess *session.Session
		var err error
		if *cookie != "" {
			sess, err = session.Parse(*cookie)
		} else {
			var raw []byte
			if raw, err = io.ReadAll(os.Stdin); err == nil {
				sess, err = session.Parse(string(raw))
			}
		}
		if err != nil {
			return err
		}
		if err := store.Save(sess); err != nil {
			return err
		}
		fmt.Printf("session stored in %s\n", store.Path())

		// Renewing straight away turns "stored" into "works".
		if err := newClient(cfg, store).RenewSession(context.Background()); err != nil {
			return fmt.Errorf("stored, but it cannot mint a token: %w", err)
		}
		fmt.Println("renewed once, so the session works")
		printSession(store.Current())
		return nil

	case "show":
		sess, err := store.Load()
		if err != nil {
			return err
		}
		printSession(sess)
		return nil

	case "refresh":
		if _, err := store.Load(); err != nil {
			return err
		}
		if err := newClient(cfg, store).RenewSession(context.Background()); err != nil {
			return err
		}
		fmt.Println("renewed")
		printSession(store.Current())
		return nil

	default:
		return fmt.Errorf("unknown session subcommand %q", args[0])
	}
}

func printSession(sess *session.Session) {
	fmt.Printf("cookie:    %s\n", sess.CookieName)
	if claims, ok := sess.Claims(); ok {
		fmt.Printf("user:      %s\n", claims.Sub)
		fmt.Printf("device:    %s\n", sess.Device())
		fmt.Printf("token:     %s left\n", time.Until(time.Unix(claims.Exp, 0)).Round(time.Second))
	} else {
		fmt.Println("token:     none minted yet")
	}
	if left, ok := sess.Renewable(); ok {
		fmt.Printf("renewable: %s left (until %s)\n", left.Round(time.Hour), sess.Expires.Format(time.RFC3339))
	} else {
		fmt.Println("renewable: unknown until the first renewal")
	}
}

// cmdSign stays for the day Wallapop brings request signing back: given a captured call,
// it says which payload layout reproduces the signature.
func cmdSign(cfg config.Config, args []string) error {
	if len(args) < 3 {
		return errors.New("sign needs: <method> <path> <timestamp> [signature]")
	}
	method, path := args[0], args[1]
	ts, err := strconv.ParseInt(strings.TrimSpace(args[2]), 10, 64)
	if err != nil {
		return fmt.Errorf("timestamp: %w", err)
	}

	if len(args) >= 4 {
		if scheme, ok := wallapop.MatchScheme(method, path, ts, args[3]); ok {
			fmt.Printf("matches scheme %q, set WALLA_SIGN_SCHEME=%s\n", scheme, scheme)
			return nil
		}
		fmt.Println("no known scheme reproduces that signature")
		for _, scheme := range wallapop.Schemes() {
			got, err := wallapop.Sign(scheme, method, path, ts)
			if err != nil {
				return err
			}
			fmt.Printf("  %-7s %s\n", scheme, got)
		}
		return errors.New("signature not reproduced")
	}

	scheme := cfg.Scheme
	if scheme == wallapop.SchemeNone {
		scheme = wallapop.SchemePipe
	}
	sig, err := wallapop.Sign(scheme, method, path, ts)
	if err != nil {
		return err
	}
	fmt.Println(sig)
	return nil
}
