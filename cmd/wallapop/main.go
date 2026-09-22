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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/commands"
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
		return cmdSearches(cfg, store, args[1:])
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

	// One store of silenced searches for the whole process: the round reads what the
	// buttons write.
	mutes, err := watch.LoadMutes(cfg.DataDir)
	if err != nil {
		return err
	}

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

	// The jobs keep their own clocks: the catalogue is a daily errand, the searches are
	// checked on a short random one so the pattern is not a metronome, and the bot answers
	// whenever it is asked.
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
			res := watchPass(ctx, cfg, store, log, watch.Options{All: cfg.WatchAll, Mutes: mutes})

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

	if bot := telegram.New(cfg.TelegramToken, cfg.TelegramChat); bot.Enabled() {
		listener := &commands.Listener{
			Bot:      bot,
			Chat:     cfg.TelegramChat,
			OnButton: onButton(cfg, store, mutes),
			Log:      log,
		}
		listener.Commands = botCommands(cfg, store, log, listener, mutes,
			func() time.Time { return time.Unix(nextWatch.Load(), 0) },
			func() time.Time { return time.Unix(next.Load(), 0) })
		go func() {
			if err := listener.Serve(ctx); err != nil {
				log.Error("the bot stopped listening", "err", err)
			}
		}()
	}

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

	mutes, err := watch.LoadMutes(cfg.DataDir)
	if err != nil {
		return err
	}
	res := watchPass(ctx, cfg, store, log, watch.Options{DryRun: *dryRun, All: *all, Mutes: mutes})
	fmt.Println(res.Summary())
	if !res.OK() {
		return errors.New("the round did not finish clean")
	}
	return nil
}

func cmdSearches(cfg config.Config, store *session.Store, args []string) error {
	fs := flag.NewFlagSet("searches", flag.ExitOnError)
	short := fs.Bool("short", false, "only the watched ones, as the bot answers them")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mutes, err := watch.LoadMutes(cfg.DataDir)
	if err != nil {
		return err
	}
	rows, err := searchRows(context.Background(), cfg, store, mutes)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no hay busquedas guardadas en la cuenta")
		return nil
	}
	if *short {
		fmt.Println(htmlSearches(rows))
		return nil
	}
	fmt.Println(plainSearches(rows))
	return nil
}

// searchRow is one saved search as both readers need it: the terminal wants the query, the
// phone wants to know whether it will hear from it.
type searchRow struct {
	Name     string
	Location string
	Query    string
	// Off is the alert switched off in the app; Muted is switched off from the bot.
	Off   bool
	Muted bool
}

func searchRows(ctx context.Context, cfg config.Config, store *session.Store, mutes *watch.Mutes) ([]searchRow, error) {
	if _, err := store.Load(); err != nil {
		return nil, err
	}
	searches, err := newClient(cfg, store).SavedSearches(ctx)
	if err != nil {
		return nil, err
	}

	rows := make([]searchRow, 0, len(searches))
	for _, search := range searches {
		rows = append(rows, searchRow{
			Name:     search.Name(),
			Location: search.LocationLabel,
			Query:    search.Values().Encode(),
			Off:      !search.Alert.Enabled && !cfg.WatchAll,
			Muted:    mutes.IsMuted(search.ID),
		})
	}
	return rows, nil
}

func countRows(rows []searchRow) (watched, off, muted int) {
	for _, row := range rows {
		switch {
		case row.Off:
			off++
		case row.Muted:
			muted++
		default:
			watched++
		}
	}
	return watched, off, muted
}

// plainSearches is the terminal answer: everything, queries included, in a fixed width
// that a terminal honours.
func plainSearches(rows []searchRow) string {
	var b strings.Builder
	for _, row := range rows {
		mark := "ON  "
		switch {
		case row.Off:
			mark = "off "
		case row.Muted:
			mark = "MUTE"
		}
		fmt.Fprintf(&b, "%s %-28s %s\n", mark, row.Name, row.Location)
		fmt.Fprintf(&b, "     %s\n", row.Query)
	}

	watched, off, muted := countRows(rows)
	fmt.Fprintf(&b, "\nVigilo %d de %d.", watched, len(rows))
	if off > 0 {
		fmt.Fprintf(&b, " %d apagadas en la app.", off)
	}
	if muted > 0 {
		fmt.Fprintf(&b, " %d silenciadas desde el bot.", muted)
	}
	return b.String()
}

// htmlSearches is the phone answer. Telegram draws messages in a proportional font, so a
// column padded with spaces only lines up inside <pre>; outside it, the same padding is
// the ragged mess it was before. Queries stay on the terminal, where they are readable.
func htmlSearches(rows []searchRow) string {
	watched, off, muted := countRows(rows)
	if watched+muted == 0 {
		return fmt.Sprintf("🔎 <b>No vigilo ninguna busqueda</b>\n\n<i>Las %d que tienes guardadas estan apagadas en la app.</i>", off)
	}

	width := 0
	for _, row := range rows {
		if !row.Off && len(row.Name) > width {
			width = len(row.Name)
		}
	}
	if width > nameColumn {
		width = nameColumn
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔎 <b>Vigilo %d de %d busquedas</b>\n<pre>\n", watched, len(rows))
	for _, row := range rows {
		if row.Off {
			continue
		}
		// One emoji per row, so whatever width the font gives it, every row is pushed by
		// the same amount and the columns still line up.
		bell := "🔔"
		if row.Muted {
			bell = "🔕"
		}
		fmt.Fprintf(&b, "%s  %s  %s\n", bell,
			telegram.Escape(pad(row.Name, width)), telegram.Escape(row.Location))
	}
	b.WriteString("</pre>")

	b.WriteString("<i>")
	if off > 0 {
		fmt.Fprintf(&b, "%d apagadas en la app · ", off)
	}
	b.WriteString("pulsa para silenciar o devolver</i>")
	return b.String()
}

// nameColumn keeps the grid inside the width of a phone: a longer name is cut rather than
// wrapped, because a wrapped row breaks the columns of every row under it.
const nameColumn = 20

func pad(s string, width int) string {
	runes := []rune(s)
	if len(runes) > width {
		return string(runes[:width-1]) + "…"
	}
	return s + strings.Repeat(" ", width-len(runes))
}

// htmlRound is what a round looks like when it is read rather than logged: the numbers
// that changed in bold, the rest as context.
func htmlRound(res watch.Result) string {
	if res.Error != "" {
		return "⚠️ <b>La ronda ha fallado</b>\n" + telegram.Escape(res.Error)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔎 <b>Ronda de las %s</b>\n", res.StartedAt.Format("15:04"))
	fmt.Fprintf(&b, "%d anuncios · <b>%d nuevos</b> · %d repetidos\n", res.Scanned, len(res.New), res.Duplicates)
	if len(res.Cheaper) > 0 {
		fmt.Fprintf(&b, "<b>%d han bajado de precio</b>\n", len(res.Cheaper))
	}

	fmt.Fprintf(&b, "%d busquedas vigiladas", res.Watched)
	if res.Silenced > 0 {
		fmt.Fprintf(&b, ", %d silenciadas", res.Silenced)
	}
	if res.Ignored > 0 {
		fmt.Fprintf(&b, ", %d apagadas", res.Ignored)
	}
	if res.Seeded > 0 {
		fmt.Fprintf(&b, "\n<i>%d apuntados sin avisar: ya estaban ahi</i>", res.Seeded)
	}
	for _, f := range res.Failures {
		fmt.Fprintf(&b, "\n⚠️ %s: %s", telegram.Escape(f.Search), telegram.Escape(f.Error))
	}
	return b.String()
}

// statusReport is the service in the three blocks that answer "is this alive": the round,
// the catalogue and the session.
func statusReport(cfg config.Config, store *session.Store, mutes *watch.Mutes, nextWatch, nextRun time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 <b>wallapop-manager</b> <code>%s</code>\n", buildVersion)

	if res, ok := watch.LoadResult(cfg.DataDir); ok {
		b.WriteString("\n" + htmlRound(res) + "\n")
	}
	if !nextWatch.IsZero() {
		fmt.Fprintf(&b, "<i>proxima ronda a las %s</i>\n", nextWatch.Format("15:04"))
	}
	if n := mutes.Count(); n > 0 {
		fmt.Fprintf(&b, "<i>%d silenciadas desde el bot</i>\n", n)
	}

	if res, ok := reactivate.LoadResult(cfg.DataDir); ok {
		fmt.Fprintf(&b, "\n♻️ <b>Catalogo</b> · %s\n", res.StartedAt.Format("02/01"))
		fmt.Fprintf(&b, "%d anuncios · %d caducados · <b>%d reactivados</b>\n",
			res.Catalogue, res.Expired, len(res.Reactivated))
	}
	if !nextRun.IsZero() {
		fmt.Fprintf(&b, "<i>proxima pasada el %s</i>\n", nextRun.Format("02/01 a las 15:04"))
	}

	if sess := store.Current(); sess != nil {
		if left, ok := sess.Renewable(); ok {
			fmt.Fprintf(&b, "\n🔑 <b>Sesion</b> · %.0f dias de margen", left.Hours()/24)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// watching serialises the rounds: the clock and a command can ask for one at the same
// time, and two rounds at once would announce the same listing twice.
var watching sync.Mutex

// watchPass waits its turn: the clock would rather run late than not at all.
func watchPass(ctx context.Context, cfg config.Config, store *session.Store, log *slog.Logger, opt watch.Options) watch.Result {
	watching.Lock()
	defer watching.Unlock()
	return runWatch(ctx, cfg, store, log, opt)
}

func runWatch(ctx context.Context, cfg config.Config, store *session.Store, log *slog.Logger, opt watch.Options) watch.Result {

	opt.MaxAge = cfg.WatchMaxAge
	opt.MaxAlerts = cfg.WatchMaxAlerts
	opt.PhotosPerItem = cfg.WatchPhotos
	opt.SeenTTL = cfg.SeenTTL
	opt.MinPause = cfg.WatchMinPause
	opt.MaxPause = cfg.WatchMaxPause
	opt.Drop = cfg.WatchDrop
	opt.Pages = cfg.SearchPages

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

// messenger turns a listing into the message that reaches the phone, with the one button
// worth having there: enough of this search for now.
type messenger struct{ bot *telegram.Bot }

func (m *messenger) Listing(ctx context.Context, search wallapop.SavedSearch, item wallapop.SearchItem) error {
	return m.bot.Photo(ctx, item.Photo(), watch.Line(search.Name(), item, telegram.Escape), listingKeys(search, item))
}

func (m *messenger) Cheaper(ctx context.Context, search wallapop.SavedSearch, item wallapop.SearchItem, before float64) error {
	return m.bot.Photo(ctx, item.Photo(), watch.CheaperLine(search.Name(), item, before, telegram.Escape),
		listingKeys(search, item))
}

func (m *messenger) Say(ctx context.Context, text string) error {
	return m.bot.Text(ctx, telegram.Escape(text), nil)
}

// listingKeys puts the two things a listing is for under it: opening it, and hearing less
// of that search.
func listingKeys(search wallapop.SavedSearch, item wallapop.SearchItem) *telegram.Keyboard {
	return &telegram.Keyboard{Rows: [][]telegram.Button{{
		{Text: "🔗 Ver anuncio", URL: item.URL(), Style: "primary"},
		{Text: "🔕 Silenciar", Data: buttonMute + search.ID, Style: "danger"},
	}}}
}

type printer struct{}

func (printer) Listing(_ context.Context, search wallapop.SavedSearch, item wallapop.SearchItem) error {
	fmt.Println("---")
	fmt.Println(watch.Line(search.Name(), item, telegram.Escape))
	// On the phone these two hang from buttons; on a terminal they have to be printed.
	fmt.Println("enlace:", item.URL())
	fmt.Println("foto:  ", item.Photo())
	return nil
}

func (printer) Cheaper(_ context.Context, search wallapop.SavedSearch, item wallapop.SearchItem, before float64) error {
	fmt.Println("---")
	fmt.Println(watch.CheaperLine(search.Name(), item, before, telegram.Escape))
	fmt.Println("enlace:", item.URL())
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

// The two buttons, by what they do. The id of a saved search is a 36 character uuid and
// Telegram caps callback data at 64 bytes, so the verb in front of it has to be short.
const (
	buttonToggle = commands.DataPrefix + "t:"
	buttonMute   = commands.DataPrefix + "m:"
)

// searchKeys draws one switch per search: pressing it silences the ones that speak and
// gives back the ones that were silenced.
func searchKeys(searches []wallapop.SavedSearch, mutes *watch.Mutes, all bool) *telegram.Keyboard {
	keys := &telegram.Keyboard{}
	for _, search := range searches {
		if !search.Alert.Enabled && !all {
			continue
		}
		text, style := "🔔 "+search.Name(), "success"
		if mutes.IsMuted(search.ID) {
			text, style = "🔕 "+search.Name(), "danger"
		}
		keys.Rows = append(keys.Rows, []telegram.Button{{
			Text:  text,
			Data:  buttonToggle + search.ID,
			Style: style,
		}})
	}
	if len(keys.Rows) == 0 {
		return nil
	}
	return keys
}

// onButton is the whole of what a press can do: silence a search, or give it back. It
// never writes to Wallapop, so the alert switch in the app stays where its owner left it.
func onButton(cfg config.Config, store *session.Store, mutes *watch.Mutes) func(context.Context, string) (string, *telegram.Keyboard, error) {
	return func(ctx context.Context, data string) (string, *telegram.Keyboard, error) {
		switch {
		case strings.HasPrefix(data, buttonToggle):
			id := strings.TrimPrefix(data, buttonToggle)
			searches, err := searchesByID(ctx, cfg, store)
			if err != nil {
				return "", nil, err
			}
			search, ok := searches[id]
			if !ok {
				return "esa busqueda ya no esta en la cuenta", nil, nil
			}

			muted, err := mutes.Toggle(id, search.Name())
			if err != nil {
				return "", nil, err
			}
			notice := "Vuelvo a avisarte de " + search.Name()
			if muted {
				notice = "Silenciada " + search.Name()
			}
			return notice, searchKeys(values(searches), mutes, cfg.WatchAll), nil

		case strings.HasPrefix(data, buttonMute):
			id := strings.TrimPrefix(data, buttonMute)
			searches, err := searchesByID(ctx, cfg, store)
			if err != nil {
				return "", nil, err
			}
			search, ok := searches[id]
			if !ok {
				return "esa busqueda ya no esta en la cuenta", nil, nil
			}
			if err := mutes.Mute(id, search.Name()); err != nil {
				return "", nil, err
			}
			// The button that did it becomes the label saying it is done.
			done := &telegram.Keyboard{Rows: [][]telegram.Button{{telegram.Off("🔕 " + search.Name() + " silenciada")}}}
			return "Silenciada " + search.Name() + ". Se enciende otra vez desde /wp_searches", done, nil
		}
		return "", nil, nil
	}
}

func searchesByID(ctx context.Context, cfg config.Config, store *session.Store) (map[string]wallapop.SavedSearch, error) {
	if _, err := store.Load(); err != nil {
		return nil, err
	}
	searches, err := newClient(cfg, store).SavedSearches(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]wallapop.SavedSearch, len(searches))
	for _, search := range searches {
		byID[search.ID] = search
	}
	return byID, nil
}

// values keeps the order Wallapop answers in, which is the order the app shows.
func values(byID map[string]wallapop.SavedSearch) []wallapop.SavedSearch {
	out := make([]wallapop.SavedSearch, 0, len(byID))
	for _, search := range byID {
		out = append(out, search)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// botCommands is the table the bot answers from. Names carry the wp_ prefix so another
// service can share this bot without a clash.
func botCommands(cfg config.Config, store *session.Store, log *slog.Logger, listener *commands.Listener,
	mutes *watch.Mutes, nextWatch, nextRun func() time.Time) []commands.Command {
	table := []commands.Command{
		{
			Name: commands.Prefix + "searches",
			Help: "que busquedas vigilo, y el interruptor de cada una",
			Run: func(ctx context.Context) (commands.Reply, error) {
				rows, err := searchRows(ctx, cfg, store, mutes)
				if err != nil {
					return commands.Reply{}, err
				}
				searches, err := searchesByID(ctx, cfg, store)
				if err != nil {
					return commands.Reply{}, err
				}
				return commands.Reply{Text: htmlSearches(rows), Keys: searchKeys(values(searches), mutes, cfg.WatchAll)}, nil
			},
		},
		{
			Name: commands.Prefix + "status",
			Help: "ultima ronda, proxima y estado de la sesion",
			Run: func(context.Context) (commands.Reply, error) {
				return commands.Say(statusReport(cfg, store, mutes, nextWatch(), nextRun())), nil
			},
		},
		{
			Name: commands.Prefix + "check",
			Help: "mira las busquedas ahora, sin esperar al reloj",
			Run: func(ctx context.Context) (commands.Reply, error) {
				// A command would rather be told no than queue behind a round that is
				// already doing the very thing it asked for.
				if !watching.TryLock() {
					return commands.Reply{}, commands.ErrBusy
				}
				defer watching.Unlock()
				// The new listings announce themselves; this is only the receipt.
				return commands.Say(htmlRound(runWatch(ctx, cfg, store, log, watch.Options{All: cfg.WatchAll, Mutes: mutes}))), nil
			},
		},
	}
	return append(table, commands.Command{
		Name: commands.Prefix + "help",
		Help: "esto",
		Run: func(context.Context) (commands.Reply, error) {
			return commands.Say(commands.Help(listener.Commands)), nil
		},
	})
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
