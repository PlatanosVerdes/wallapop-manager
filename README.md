# wallapop-manager

Two jobs, one binary:

- **The catalogue.** A listing on my account expires after a while and goes quiet until
  somebody presses **Reactivar** on it. This does that pass once a day, and says nothing
  unless something needs a human.
- **The searches.** A public Telegram bot. Anybody pastes the address of a search made on
  the web, the searches are replayed every few minutes, and anything genuinely new in them
  arrives in that person's chat with its photo and a link.

Runs on the Raspberry from [rpi-services](https://github.com/PlatanosVerdes/rpi-services),
same conventions as the rest: Go with no dependencies, CalVer tag on every push to main,
`/healthz` for the blackbox probe.

## The API

Wallapop's [Connect API](https://developers.wallapop.com) is open to PRO sellers and
integrators with a registered OAuth client, and documents none of this, so it uses the same
private API the web app does. Three endpoints, all captured from the browser:

| Call | Session | What for |
| :--- | :--- | :--- |
| `GET /api/v3/user/items` | yes | The catalogue. The pagination cursor comes back in the `X-NextPage` response header, not in the body |
| `PUT /api/v3/items/{id}/reactivate` | yes | Presses the button. Answers `204 No Content` |
| `GET /api/v3/search` | **no** | The catalogue search, which is how every search is replayed |

Requests carry the bearer plus the device headers a browser sends (`x-deviceid`,
`x-appversion`, `deviceos`). There is no request signing: the older `X-Signature` scheme
(HMAC-SHA256 over method, path and timestamp) is gone from the web app, and lives on here
behind `WALLA_SIGN_SCHEME` for the day it returns.

The search endpoint answers the same to anybody, so the watcher sends **no bearer and no
device id** on it, and needs no session at all: the session is the catalogue's alone.

The web search page and the API share their parameter names, so a pasted address is the
query almost as it stands. Three things about it are not obvious:

| Parameter | Behaviour |
| :--- | :--- |
| `source` | Required. Without it the API answers `400`; any value is accepted |
| `distance_in_km` | The radius that filters, and only next to `latitude` and `longitude`. Without them the search covers the whole country |
| `distance` | Ignored. It is left out of the cursor the API hands back, which lists the parameters it accepted |

`order_by` is always forced to `newest`, because that is the order in which something new
is on the first page.

## The session

This is the part that needs care. The access token is a Keycloak token
(`accounts.wallapop.com`, realm `wallapop-internal`, client `web`) and **lasts five
minutes**: `exp - iat` is exactly 300 seconds. But there is no refresh token to store:
the web app is a NextAuth app, and the refresh token is encrypted inside the
`__Secure-next-auth.session-token` cookie, which is `httpOnly` and only its own server can
read. That is why renewing a session produces no traffic to `accounts.wallapop.com` at
all.

So the way to get a token is the same one the page uses, `getSession()`:

| Call | What for |
| :--- | :--- |
| `GET es.wallapop.com/api/auth/session` | Send the session cookie, get `{"token": "<access token>", "expires": "…"}` back. Their server does the Keycloak refresh |

The cookie rolls on every read, so a renewal stores the new value back and the session
outlives any single token. A revoked cookie is answered with an empty session and a 200,
not an error, which is treated as the one case that needs a human.

When an API call is rejected, the `x-wallapop-unauthorized` response header says which
half expired: `ACCESS_TOKEN_EXPIRED` is renewed and retried on the spot,
`REFRESH_TOKEN_EXPIRED` wakes a human.

Automating this is against Wallapop's terms of service. It is my own account and my own
ten listings, so the practical risk is being flagged as a bot: hence one catalogue pass a
day and a random 20-90s pause between listings. The watcher never touches the account, so
what it risks is the Pi's address being throttled by the search endpoint: see the cost of
a round below.

## Telling a new listing from the same one again

Most searches are for vehicles, and vehicles are advertised by dealer networks: one van is
posted by eleven accounts from eleven towns at eleven slightly different prices. Announcing
that is eleven messages for one van.

The photograph is what identifies a listing, not the words and not the seller. Each new
listing has its thumbnails reduced to a **dHash**: shrunk to a 9x8 grey grid, one bit per
pair of neighbouring cells saying which is brighter, 64 bits in total. The same photograph
uploaded again lands within a few bits of itself, whoever uploaded it.

So a listing is a copy of one already seen when:

| Rule | What it catches |
| :--- | :--- |
| A photo within **10 bits** and a price within **10%** | The same advert reposted from another town by another account |
| A photo within 10 bits and a price within 50%, **same seller** | His own listing uploaded again and repriced |
| Titles **60%** alike and a price within 30%, **same seller** | His own listing rewritten and photographed again |

Words alone never fold two different sellers together: two strangers selling the same white
IKEA shelf write the same title at the same price, and that is two shelves. For the same
reason a photo match has to agree on price, because both of them used the maker's catalogue
picture.

Measured on a live round of my searches: of 5.886 pairs of listings, 1.5% sat under
10 bits apart and everything else piled up above 20, so the threshold sits in an empty gap.
A thumbnail costs about 25 ms including the download, and only listings never seen before
are hashed, so a quiet round downloads nothing at all.

What has been seen is kept per chat in `data/users/<chat>/seen.json`, so a listing
announced to one person is still news to another, and it is forgotten after
`WALLA_SEEN_TTL`. The first round of a new search announces nothing: everything in it was
already there.

## When something gets cheaper

A listing already seen is not news, but the same listing at a lower price is: that is what
a watched search is for. Every round compares what a known listing costs now against the
lowest it has ever been while watched, and a fall of `WALLA_WATCH_DROP` or more is
announced with what it used to cost.

Measuring against the lowest and not against yesterday is what keeps a seller who bounces
between two numbers from being announced every week: the second time a price reaches a low
it has already been at, it is not news any more.

The duplicate rule carries over whole. A listing folded as a copy is recorded as a copy
**of** the one that was announced, and a copy never speaks: when the dealer network
reprices its eleven accounts, the message is the one listing that was announced, once.

## The cost of a round

Every request of a round leaves from the Pi's address, so the number of them is what gets
it throttled. A page is 40 listings and a search ordered by newest has anything new on its
first page, so an ordinary round reads **one page per search**. Once every
`WALLA_DEEP_EVERY` a round reads `WALLA_SEARCH_PAGES` pages instead, and so does the first
round of a new search: that is what keeps the prices further down a search watched, and
what records a new search whole before it starts talking.

A search is asked once per round however many chats watch it: each chat compares the same
answer against its own memory. The round's summary and `wallapop_watch_shared` say how
many were answered that way.

With five people and `WALLA_MAX_SEARCHES` at 3, an ordinary round is at most 15 requests
and a deep one 45, plus the thumbnails of listings never seen before.

## The bot

A bot of its own, for this and nothing else, and open: `/start` is all it takes to join,
up to `WALLA_MAX_USERS` chats, because every search is requests from the Pi. A chat that
has not sent `/start` gets nothing else answered.

Each chat sees its own searches and its own listings, and nothing else: not the other
chats, and nothing of the account the catalogue runs on.

A search is added by pasting the address of a search made on es.wallapop.com, with any
text around it, before or after, taken as its name. The answer says the radius, which matters because a
search made without a location covers the whole country.

| Command | What it does |
| :--- | :--- |
| `/busquedas` | Your searches, each with a bell that silences it or gives it back, a pencil that renames it, and a bin that deletes it after asking |
| `/nueva [name] <address>` | The same as pasting the address |
| `/ahora` | Runs one of your searches now, or all of them: a button per search and one for all, straight to the search when there is only one. A search picked by name is read even when silenced. Answers `ya estoy buscando` rather than queueing behind a round already running |
| `/baja` | Deletes your searches and what was seen for you, after asking |
| `/ayuda` | The list, built from the same table the bot dispatches from |

The pencil asks for the new name and takes the next message as it, for five minutes; an
address sent instead is still a new search. The question lives in memory, so one lost to
a restart is asked again by pressing the pencil.

Silencing keeps the search and stops the messages. Deleting forgets it: the same address
pasted again starts over, first round silent.

Every announced listing carries two buttons, **open the listing** and **silence this
search**, because the moment it is clear that a search is talking too much is the moment
one of its messages arrives. The address rides on the button rather than in the text.

Every press is looked up inside the chat it came from, so a forged button cannot reach
somebody else's search.

A command sent while the container is being replaced is not lost: the queue handed over on
the first read is judged by the age of each message, and anything sent within two minutes
still gets its answer. A press has no time of its own, so the queue's presses are left
alone instead: one lost to a restart is pressed again, and the button shows which way it
went.

A button press is a `callback_query`, which is only delivered when `allowed_updates` asks
for it, carries at most 64 bytes of `callback_data` (so the verb is one letter and a search
id is 8 characters), and must be answered with `answerCallbackQuery` or the phone spins
until the press expires. The keyboard is then redrawn in place with
`editMessageReplyMarkup`.

A bot token has exactly one reader: Telegram hands each update to whoever asks first and
answers a second one with a 409. This service is that reader, which is why the bot is not
shared with anything else.

## Commands

```bash
wallapop run --dry-run   # list what would be reactivated, touch nothing
wallapop run             # one catalogue pass
wallapop watch --dry-run # one round of everybody's searches, printing what would be announced
wallapop watch           # one round, announcing on Telegram (--deep, --chat <id>)
wallapop searches        # who uses the bot, and each of their searches as an address
wallapop searches add --chat <id> [--name <name>] '<address>'
wallapop serve           # daily pass, the watcher, the bot, and /healthz on :8000
wallapop session import --cookie '<value>'   # store the browser session cookie
wallapop session show    # what is stored and whether it can renew itself
wallapop session refresh # renew now, which is how you check it works
```

## Importing a session

In DevTools, **Application → Cookies → `https://es.wallapop.com`**, copy the value of
`__Secure-next-auth.session-token`:

```bash
wallapop session import --cookie '<the cookie value>'
```

The import renews once before reporting success, so "stored" and "works" are the same
thing. `session refresh` repeats that check at any time, and `session show` reports how
long the cookie has left.

## Configuration

| Variable | Default | What it does |
| :--- | :--- | :--- |
| `WALLA_DATA_DIR` | `./data` | Where the session, the users, the last runs and what has been seen are kept |
| `WALLA_INTERVAL` | `24h` | Time between catalogue passes in `serve`. Under a minute is refused |
| `WALLA_RETRY_EVERY` | `15m` | How soon a failed pass is tried again. Under a minute is refused |
| `WALLA_PORT` | `8000` | Port for `/healthz` |
| `WALLA_MIN_PAUSE` / `WALLA_MAX_PAUSE` | `20s` / `90s` | Random pause between listings when reactivating |
| `WALLA_MAX_PER_RUN` | `25` | Ceiling on listings touched in one pass |
| `WALLA_WARN_BEFORE` | `72h` | How early the end of the session is announced |
| `WALLA_PUSHGATEWAY` | – | Pushgateway base URL. Empty means report nothing, which is right off the Pi |

The watcher:

| Variable | Default | What it does |
| :--- | :--- | :--- |
| `WALLA_TELEGRAM_TOKEN` | – | Bot token. Empty means the watcher runs and announces nothing |
| `WALLA_TELEGRAM_CHAT` | – | The owner's chat, always a user of the bot |
| `WALLA_MAX_SEARCHES` | `3` | Searches one chat may keep |
| `WALLA_MAX_USERS` | `20` | Chats that may join |
| `WALLA_WATCH_MIN` / `WALLA_WATCH_MAX` | `5m` / `15m` | The round is run after a random wait in this range, so the pattern is not a metronome |
| `WALLA_WATCH_MAX_AGE` | `24h` | A listing older than this is recorded without a message: it was already there |
| `WALLA_WATCH_MAX_ALERTS` | `10` | Messages per round. The rest are counted in one line at the end |
| `WALLA_WATCH_PHOTOS` | `2` | Thumbnails hashed per new listing |
| `WALLA_WATCH_DROP` | `5` | Percent a listing has to shed, against its own lowest price, before the fall is announced. `0` says nothing about prices |
| `WALLA_SEARCH_PAGES` | `3` | Pages of each search read on a deep round. One page is 40 listings |
| `WALLA_DEEP_EVERY` | `1h` | How often a round reads every page instead of the first one |
| `WALLA_WATCH_MIN_PAUSE` / `WALLA_WATCH_MAX_PAUSE` | `3s` / `15s` | Random pause between searches within a round |
| `WALLA_SEEN_TTL` | `720h` | How long a listing is remembered for the duplicate check |

The endpoints, so a change on their side is a redeploy and not a rebuild:

| Variable | Default |
| :--- | :--- |
| `WALLA_PATH_ITEMS` | `/api/v3/user/items` |
| `WALLA_PATH_REACTIVATE` | `/api/v3/items/%s/reactivate` |
| `WALLA_REACTIVATE_METHOD` | `PUT` |
| `WALLA_PATH_SEARCH` | `/api/v3/search` |
| `WALLA_BASE_URL` | `https://api.wallapop.com` |
| `WALLA_WEB_URL` | `https://es.wallapop.com` |
| `WALLA_SIGN_SCHEME` | `none`, or `pipe` / `legacy` if they ever sign requests again |
| `WALLA_DEVICE_ID` | from the `device_id` claim |
| `WALLA_APP_VERSION` | `826680` |
| `WALLA_LOG_JSON` | `1` for JSON logs, which is what Vector collects |

## Health

`/healthz` answers **200 whenever the process is alive**, and the body carries the state:

```json
{
  "status": "ok",
  "session": "ok",
  "renewable_days_left": 27.4,
  "next_run": "2026-09-23T09:00:00+02:00",
  "last_run": { "catalogue": 10, "expired": 6, "reactivated": ["Apple Watch SE 2 44mm"] },
  "next_watch": "2026-09-22T11:19:00+02:00",
  "last_watch": { "users": 3, "requests": 4, "shared": 1, "watched": 5, "scanned": 200, "duplicates": 21, "new": [] }
}
```

`status` is `down` when there is no session or the cookie has expired, `warn` when it ends
within `WALLA_WARN_BEFORE` or the last pass failed, and `ok` otherwise. A failed round of
searches does not change it: the next round is minutes away.

Both jobs report as gauges, and the alert rules decide what is worth waking somebody for:
`wallapop_last_run_status`, `wallapop_session_days_remaining`, `wallapop_expired_listings`,
`wallapop_reactivated_listings`, and for the watcher `wallapop_watch_status`,
`wallapop_watch_users`, `wallapop_watch_searches`, `wallapop_watch_requests`,
`wallapop_watch_shared`, `wallapop_watch_scanned`,
`wallapop_watch_new`, `wallapop_watch_duplicates`, `wallapop_watch_cheaper`.

Telegram carries the listings, and the answers to whoever asked. The state of the service
is a metric.
