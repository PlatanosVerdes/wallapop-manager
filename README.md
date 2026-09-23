# wallapop-manager

One binary, two jobs:

- **[@WuallaBot](https://t.me/WuallaBot)**: a public Telegram bot that tells you when something new appears in your Wallapop searches.
- **Catalogue**: reactivates my expired Wallapop listings once a day.

Runs on the Raspberry from [rpi-services](https://github.com/PlatanosVerdes/rpi-services). Go, no dependencies, CalVer tag on every push to main.

## The bot

1. `/start` to join.
2. Write what you are looking for. The bot shows what it understood and saves it on ✅ (a greeting gets a joke instead). `/nueva <search>` saves it without asking, and `/nueva` alone waits for the next message. A price and a radius are picked out of the text:

   | Written | Searches |
   | :--- | :--- |
   | `iphone 13` | "iphone 13", all of Spain |
   | `kallax hasta 40` · `máx 40` · `menos de 40` | up to 40 € |
   | `moto desde 1.500` · `más de 1500` | from 1500 € |
   | `bici 100-300` · `entre 100 y 300` | 100 to 300 € |
   | `sofá en Sant Cugat` | 30 km around Sant Cugat |
   | `sofá en Sant Cugat a 10 km` | 10 km around Sant Cugat |

   The town after the last `en` is looked up on OpenStreetMap; when it is no town (`funda en piel`), it stays part of the text.
3. Optional: send a location (📎 → Location, any point on the map) to move the latest search there, 30 km unless the text said otherwise.

A listing shared from the app (`wallapop.com/item/...`) becomes a search for things like it: the first three words of its title, in its category, up to 20% dearer. It is shown for ✅ too.

For filters the text cannot say (category, condition, brand and model), make the search on es.wallapop.com in a phone browser (typed in the address bar, so it does not jump to the app) and paste its address. Whatever is written next to it is the name, e.g. `coches top https://es.wallapop.com/search?...`

| Command | What it does |
| :--- | :--- |
| `/nueva <search>` | Same as writing it, or pasting the address |
| `/busquedas` | Your searches: 🔔 silence, ✏️ rename, 🗑 delete |
| `/ahora` | Check one search, or all, right now |
| `/baja` | Leave and delete your searches |
| `/ayuda` | Help |

Each listing arrives with its photo, price, town, and two buttons: open it, or silence that search.

Each chat only sees its own searches. Up to `WALLA_MAX_USERS` chats, `WALLA_MAX_SEARCHES` searches each.

## How it works

- A round runs every 5 to 15 minutes, at random.
- Each search reads **one page** (40 listings, newest first). Every hour, and on a new search's first round, it reads 3.
- Two chats with the **same search** share one request.
- The **first round** of a new search is silent: those listings were already there.
- **Duplicates** (the same vehicle posted by many dealer accounts) are detected by photo: a 64-bit image hash plus a similar price.
- A known listing that gets **5% cheaper** than its lowest price is announced again.
- Searches need no Wallapop account. Only the catalogue uses my session.

## Search API notes

The bot uses Wallapop's private `GET /api/v3/search`, which needs no login.

| Parameter | Note |
| :--- | :--- |
| `source` | Required, any value (400 without it) |
| `distance_in_km` | The radius. Only works with `latitude` and `longitude`; without them, all of Spain |
| `distance` | Ignored by the API |
| `order_by` | Always forced to `newest` |

## Catalogue and session

Reactivates expired listings with `GET /api/v3/user/items` and `PUT /api/v3/items/{id}/reactivate`, once a day, with a random pause between listings.

The session is the browser cookie `__Secure-next-auth.session-token`. The service renews the 5-minute token from it on its own; the cookie lasts about a month. To import it, copy it from DevTools (Application → Cookies → es.wallapop.com):

```bash
wallapop session import --cookie '<value>'
```

## Data

In `WALLA_DATA_DIR`:

| File | Holds |
| :--- | :--- |
| `searches.json` | Every chat and its searches |
| `users/<chat>/seen.json` | What that chat has already been told |
| `session.json` | The catalogue session |
| `last_run.json`, `last_watch.json` | Last catalogue pass and last round |

## CLI

```bash
wallapop serve                          # everything: bot, rounds, daily pass, /healthz
wallapop watch --dry-run                # one round, print instead of send
wallapop searches                       # every chat and its searches
wallapop searches add --chat <id> '<address or text>'
wallapop run --dry-run                  # what the catalogue pass would reactivate
wallapop session show | refresh         # check the session
```

## Configuration

| Variable | Default | |
| :--- | :--- | :--- |
| `WALLA_TELEGRAM_TOKEN` | – | Bot token |
| `WALLA_TELEGRAM_CHAT` | – | Owner's chat |
| `WALLA_MAX_USERS` | `20` | Chats that can join |
| `WALLA_MAX_SEARCHES` | `4` | Searches per chat |
| `WALLA_WATCH_MIN` / `MAX` | `5m` / `15m` | Wait between rounds |
| `WALLA_WATCH_MIN_PAUSE` / `MAX_PAUSE` | `3s` / `15s` | Pause between requests |
| `WALLA_SEARCH_PAGES` | `3` | Pages on a deep round |
| `WALLA_DEEP_EVERY` | `1h` | How often a round is deep |
| `WALLA_WATCH_MAX_AGE` | `24h` | Older listings are not announced |
| `WALLA_WATCH_MAX_ALERTS` | `10` | Messages per chat per round |
| `WALLA_WATCH_PHOTOS` | `2` | Photos hashed per listing |
| `WALLA_WATCH_DROP` | `5` | % drop to announce a cheaper listing (`0` = off) |
| `WALLA_SEEN_TTL` | `720h` | How long a listing is remembered |
| `WALLA_PLACES_URL` | Nominatim | Geocoder for `en <town>` (empty = off) |
| `WALLA_INTERVAL` | `24h` | Time between catalogue passes |
| `WALLA_RETRY_EVERY` | `15m` | Retry after a failed pass |
| `WALLA_MIN_PAUSE` / `MAX_PAUSE` | `20s` / `90s` | Pause between reactivations |
| `WALLA_MAX_PER_RUN` | `25` | Listings touched per pass |
| `WALLA_WARN_BEFORE` | `72h` | `/healthz` warns this long before the cookie expires |
| `WALLA_DATA_DIR` | `./data` | Data folder |
| `WALLA_PORT` | `8000` | `/healthz` port |
| `WALLA_PUSHGATEWAY` | – | Where metrics go |
| `WALLA_LOG_JSON` | – | `1` for JSON logs |

Endpoints can be overridden without a rebuild: `WALLA_BASE_URL`, `WALLA_WEB_URL`, `WALLA_PATH_SEARCH`, `WALLA_PATH_ITEMS`, `WALLA_PATH_REACTIVATE`, `WALLA_REACTIVATE_METHOD`, `WALLA_SIGN_SCHEME`, `WALLA_DEVICE_ID`, `WALLA_APP_VERSION`.

## Health

`/healthz` always answers 200 while the process runs. Its `status` is `down` without a valid session, `warn` when the cookie is about to expire or the last pass failed, `ok` otherwise.

Metrics go to the Pushgateway as `wallapop_*` gauges (catalogue) and `wallapop_watch_*` gauges (bot rounds).
