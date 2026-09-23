// Package config reads everything from the environment so compose is the only place
// where this service is configured.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

type Config struct {
	DataDir    string
	BaseURL    string
	WebURL     string
	Scheme     wallapop.SignScheme
	DeviceID   string
	AppVersion string

	Interval time.Duration
	// RetryEvery is how soon a failed pass is tried again.
	RetryEvery time.Duration
	Port       int

	MinPause  time.Duration
	MaxPause  time.Duration
	MaxPerRun int

	// Pushgateway is empty when there is nothing to report to, which is the case
	// outside the Pi.
	Pushgateway string
	WarnBefore  time.Duration

	// The watcher: the bot, the users' searches, and what it is allowed to say.
	TelegramToken string
	// TelegramChat is the owner's chat: it approves who joins, and it is the only one told
	// about the catalogue and the session.
	TelegramChat string
	// MaxSearches is how many searches one chat may keep.
	MaxSearches    int
	WatchMin       time.Duration
	WatchMax       time.Duration
	WatchMaxAge    time.Duration
	WatchMaxAlerts int
	WatchPhotos    int
	// SearchPages is how many pages of each search a deep round reads. One page is 40.
	SearchPages int
	// DeepEvery is how often a round reads every page instead of the first one, which is
	// what keeps the prices further down a search watched.
	DeepEvery time.Duration
	// WatchDrop is the share of its own lowest price a listing has to shed before the
	// fall is worth a message. Zero says nothing about prices.
	WatchDrop     float64
	WatchMinPause time.Duration
	WatchMaxPause time.Duration
	SeenTTL       time.Duration

	LogJSON bool
}

func Load() (Config, error) {
	cfg := Config{
		DataDir:     env("WALLA_DATA_DIR", "./data"),
		BaseURL:     env("WALLA_BASE_URL", wallapop.DefaultBaseURL),
		WebURL:      env("WALLA_WEB_URL", wallapop.DefaultWebURL),
		Scheme:      wallapop.SignScheme(env("WALLA_SIGN_SCHEME", string(wallapop.SchemeNone))),
		DeviceID:    env("WALLA_DEVICE_ID", ""),
		AppVersion:  env("WALLA_APP_VERSION", wallapop.DefaultAppVersion),
		Interval:    duration("WALLA_INTERVAL", 24*time.Hour),
		RetryEvery:  duration("WALLA_RETRY_EVERY", 15*time.Minute),
		Port:        number("WALLA_PORT", 8000),
		MinPause:    duration("WALLA_MIN_PAUSE", 20*time.Second),
		MaxPause:    duration("WALLA_MAX_PAUSE", 90*time.Second),
		MaxPerRun:   number("WALLA_MAX_PER_RUN", 25),
		Pushgateway: env("WALLA_PUSHGATEWAY", ""),
		WarnBefore:  duration("WALLA_WARN_BEFORE", 72*time.Hour),

		TelegramToken:  env("WALLA_TELEGRAM_TOKEN", ""),
		TelegramChat:   env("WALLA_TELEGRAM_CHAT", ""),
		MaxSearches:    number("WALLA_MAX_SEARCHES", 3),
		WatchMin:       duration("WALLA_WATCH_MIN", 5*time.Minute),
		WatchMax:       duration("WALLA_WATCH_MAX", 15*time.Minute),
		WatchMaxAge:    duration("WALLA_WATCH_MAX_AGE", 24*time.Hour),
		WatchMaxAlerts: number("WALLA_WATCH_MAX_ALERTS", 10),
		WatchPhotos:    number("WALLA_WATCH_PHOTOS", 2),
		SearchPages:    number("WALLA_SEARCH_PAGES", 3),
		DeepEvery:      duration("WALLA_DEEP_EVERY", time.Hour),
		WatchDrop:      percent("WALLA_WATCH_DROP", 5) / 100,
		WatchMinPause:  duration("WALLA_WATCH_MIN_PAUSE", 3*time.Second),
		WatchMaxPause:  duration("WALLA_WATCH_MAX_PAUSE", 15*time.Second),
		SeenTTL:        duration("WALLA_SEEN_TTL", 30*24*time.Hour),

		LogJSON: env("WALLA_LOG_JSON", "") == "1",
	}

	// A wait of zero would turn the ticker into a spin loop, which is exactly what an
	// unset field did on 2026-09-03: 7887 passes in a minute.
	if cfg.RetryEvery < time.Minute {
		return cfg, fmt.Errorf("WALLA_RETRY_EVERY (%s) is under a minute, which would spin", cfg.RetryEvery)
	}
	if cfg.Interval < time.Minute {
		return cfg, fmt.Errorf("WALLA_INTERVAL (%s) is under a minute, which would spin", cfg.Interval)
	}
	if cfg.MaxPause < cfg.MinPause {
		return cfg, fmt.Errorf("WALLA_MAX_PAUSE (%s) is below WALLA_MIN_PAUSE (%s)", cfg.MaxPause, cfg.MinPause)
	}
	if cfg.WatchMin < time.Minute {
		return cfg, fmt.Errorf("WALLA_WATCH_MIN (%s) is under a minute, which would spin", cfg.WatchMin)
	}
	if cfg.WatchMax < cfg.WatchMin {
		return cfg, fmt.Errorf("WALLA_WATCH_MAX (%s) is below WALLA_WATCH_MIN (%s)", cfg.WatchMax, cfg.WatchMin)
	}
	if cfg.WatchMaxPause < cfg.WatchMinPause {
		return cfg, fmt.Errorf("WALLA_WATCH_MAX_PAUSE (%s) is below WALLA_WATCH_MIN_PAUSE (%s)", cfg.WatchMaxPause, cfg.WatchMinPause)
	}
	if !wallapop.ValidScheme(cfg.Scheme) {
		return cfg, fmt.Errorf("WALLA_SIGN_SCHEME %q is not one of none, pipe, legacy", cfg.Scheme)
	}

	// A change on their side should be a redeploy, not a rebuild.
	if v := os.Getenv("WALLA_PATH_ITEMS"); v != "" {
		wallapop.PathItems = v
	}
	if v := os.Getenv("WALLA_PATH_REACTIVATE"); v != "" {
		wallapop.PathReactivate = v
	}
	if v := os.Getenv("WALLA_REACTIVATE_METHOD"); v != "" {
		wallapop.ReactivateMethod = v
	}
	if v := os.Getenv("WALLA_PATH_SEARCH"); v != "" {
		wallapop.PathSearch = v
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func number(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}

// percent reads a whole percentage, because "5" is how a drop is talked about and 0.05 is
// not.
func percent(key string, fallback float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

func duration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}
