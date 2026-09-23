# wallapop-manager

Two jobs, one binary:

- **The searches bot.** [@WuallaBot](https://t.me/WuallaBot) is a public Telegram bot.
  Anybody pastes the address of a search made on es.wallapop.com, the searches are replayed
  every few minutes, and anything genuinely new in them arrives in that person's chat with
  its photo and a link.
- **The catalogue.** A listing on my account expires after a while and goes quiet until
  somebody presses **Reactivar** on it. This does that pass once a day, and says nothing
  unless something needs a human.

Runs on the Raspberry from [rpi-services](https://github.com/PlatanosVerdes/rpi-services),
same conventions as the rest: Go with no dependencies, CalVer tag on every push to main,
`/healthz` for the blackbox probe.

## Using the bot

`/start` is all it takes to join, up to `WALLA_MAX_USERS` chats. A chat that has not sent
it gets nothing else answered. Each chat sees its own searches and its own listings, and
nothing else: not the other chats, and nothing of the account the catalogue runs on.

A search is added by making it on es.wallapop.com with whatever filters, and pasting the
address of the page into the chat. Any text around the address, before or after, is its
name: `coches top https://es.wallapop.com/search?...` is a search called "coches top".
Without a name it takes the keywords, or the make and model. The answer says the radius and
the price range, because a search made without a location covers the whole country. A chat
keeps up to `WALLA_MAX_SEARCHES` searches.

| Command | What it does |
| :--- | :--- |
| `/nueva [name] <address>` | The same as pasting the address |
| `/busquedas` | Your searches, each with a bell that silences it or gives it back, a pencil that renames it, and a bin that deletes it after asking |
| `/ahora` | Runs one of your searches now: a button per search and one for all of them, straight to the search when there is only one. A search picked by name is read even when silenced. The pressed button turns into the receipt |
| `/baja` | Deletes your searches and what was seen for you, after asking |
| `/ayuda` | What the bot does, how to add a search, and the commands |

Every announced listing is its photo, the title, the price and the town, with two buttons
under it: **open the listing** and **silence this search**. The moment a search is clearly
talking too much is the moment one of its messages arrives. A price drop is the same card
with what it used to cost.

Silencing keeps the search and stops its messages. Deleting forgets it: the same address
pasted again starts over, first round silent. The pencil asks for the new name and takes
the next message as it, for five minutes; an address sent instead is still a new search.

## How the watcher works

A round runs after a random wait between `WALLA_WATCH_MIN` and `WALLA_WATCH_MAX`, so the
pattern is not a metronome. It goes through every chat, replays each of its searches that
is not silenced, and compares what comes back against what that chat has already seen.

### Asking Wallapop as little as possible

Every request of a round leaves from the Pi's address, so the number of them is what gets
it throttled.

- **One page per search.** A page is 40 listings and a search is always ordered by newest,
  so anything new is on the first page. Once every `WALLA_DEEP_EVERY` a round reads
  `WALLA_SEARCH_PAGES` pages instead, and so does the first round of a new search: that is
  what keeps the prices further down a search watched.
- **A shared search is asked once.** Two chats watching the same query get the same answer,
  each compared against its own memory. The pause between searches only spaces the
  requests that actually go out.
- **Thumbnails only for strangers.** Only a listing never seen before is hashed, so a
  quiet round downloads no pictures at all.

With five people and three searches each, an ordinary round is at most 15 requests and a
deep one 45. The round's summary and `wallapop_watch_requests` / `wallapop_watch_shared`
say how many went out and how many were shared.

### What is not news

- **The first round of a new search** records everything in silence: it was already there.
- **A listing older than `WALLA_WATCH_MAX_AGE`** is recorded in silence too, even the first
  time it is seen.
- **More than `WALLA_WATCH_MAX_ALERTS` in one round** for one chat: the rest are counted in
  one line instead of sent.
- **A copy of a listing already seen**, which is the next section.

### Telling a new listing from the same one again

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
| A photo within 10 bits and a price within 50%, **same seller** | The seller's own listing uploaded again and repriced |
| Titles **60%** alike and a price within 30%, **same seller** | The seller's own listing rewritten and photographed again |

Words alone never fold two different sellers together: two strangers selling the same white
IKEA shelf write the same title at the same price, and that is two shelves. For the same
reason a photo match has to agree on price, because both of them used the maker's catalogue
picture.

Measured on a live round: of 5.886 pairs of listings, 1.5% sat under 10 bits apart and
everything else piled up above 20, so the threshold sits in an empty gap. A thumbnail costs
about 25 ms including the download.

### When something gets cheaper

A listing already seen is not news, but the same listing at a lower price is. Every round
compares what a known listing costs now against the lowest it has ever been while watched,
and a fall of `WALLA_WATCH_DROP` percent or more is announced with what it used to cost.

Measuring against the lowest and not against yesterday is what keeps a seller who bounces
between two numbers from being announced every week: the second time a price reaches a low
it has already been at, it is not news any more.

A listing folded as a copy is recorded as a copy **of** the one that was announced, and a
copy never speaks: when the dealer network reprices its eleven accounts, the message is the
one listing that was announced, once.

## The search API

Wallapop's [Connect API](https://developers.wallapop.com) is open to PRO sellers and
integrators with a registered OAuth client, and documents none of this, so everything here
uses the same private API the web app does.

The search is `GET /api/v3/search`, and it answers the same to anybody: the watcher sends
**no bearer and no device id**, and needs no session. The web search page and the API share
their parameter names, so a pasted address is the query almost as it stands. Three things
about it are not obvious:

| Parameter | Behaviour |
| :--- | :--- |
| `source` | Required. Without it the API answers `400`; any value is accepted |
| `distance_in_km` | The radius that filters, and only next to `latitude` and `longitude`. Without them the search covers the whole country |
| `distance` | Ignored. It is left out of the cursor the API hands back, which lists the parameters it accepted |

`order_by` is always forced to `newest`, whatever the pasted address says. The pagination
cursor is `meta.next_page` in the body.

## The catalogue and its session

The catalogue is the only part that acts on an account, and the only one that needs a
session. Two endpoints, captured from the browser:

| Call | What for |
| :--- | :--- |
| `GET /api/v3/user/items` | The catalogue. The pagination cursor comes back in the `X-NextPage` response header, not in the body |
| `PUT /api/v3/items/{id}/reactivate` | Presses the button. Answers `204 No Content` |

Requests carry the bearer plus the device headers a browser sends (`x-deviceid`,
`x-appversion`, `deviceos`). There is no request signing: the older `X-Signature` scheme
(HMAC-SHA256 over method, path and timestamp) is not used by the web app, and is kept behind
`WALLA_SIGN_SCHEME` in case it comes back.

The access token is a Keycloak token (`accounts.wallapop.com`, realm `wallapop-internal`,
client `web`) and **lasts five minutes**: `exp - iat` is exactly 300 seconds. There is no
refresh token to store: the web app is a NextAuth app, and the refresh token is encrypted
inside the `__Secure-next-auth.session-token` cookie, which is `httpOnly` and only its own
server can read. So a token is obtained the way the page does it, `getSession()`:

| Call | What for |
| :--- | :--- |
| `GET es.wallapop.com/api/auth/session` | Send the session cookie, get `{"token": "<access token>", "expires": "…"}` back. Their server does the Keycloak refresh |

The cookie rolls on every read, so a renewal stores the new value back and the session
outlives any single token. A revoked cookie is answered with an empty session and a 200,
not an error, which is treated as the one case that needs a human. When an API call is
rejected, the `x-wallapop-unauthorized` response header says which half expired:
`ACCESS_TOKEN_EXPIRED` is renewed and retried on the spot, `REFRESH_TOKEN_EXPIRED` needs a
human.

Automating an account is against Wallapop's terms of service. It is my own account and my
own few listings, so the practical risk is being flagged as a bot: hence one pass a day and
a random 20-90s pause between listings.

### Importing a session

In DevTools, **Application → Cookies → `https://es.wallapop.com`**, copy the value of
`__Secure-next-auth.session-token`:

```bash
wallapop session import --cookie '<the cookie value>'
```

The import renews once before reporting success, so "stored" and "works" are the same
thing. `session refresh` repeats that check at any time, and `session show` reports how
long the cookie has left.

## Data

Everything lives in `WALLA_DATA_DIR`:

| Path | What it holds |
| :--- | :--- |
| `searches.json` | Every chat that joined, and its searches: name, query, silenced or not |
| `users/<chat>/seen.json` | What that chat has already been told about, for the duplicate check and the price drops. Forgotten after `WALLA_SEEN_TTL` |
| `session.json` | The catalogue's session cookie and the last access token |
| `last_run.json` / `last_watch.json` | The last catalogue pass and the last round, which `/healthz` reports |

`searches.json` is read again whenever another process writes it, so a search added from
the terminal reaches the running bot without a restart.

## The Telegram side

A bot token has exactly one reader: Telegram hands each update to whoever asks first and
answers a second one with a 409. This service is that reader, which is why the bot is not
shared with anything else.

- **Every press is looked up inside the chat it came from**, so a forged button cannot reach
  somebody else's search.
- **A command sent during a deploy is not lost.** The queue handed over on the first read is
  judged by the age of each message, and anything sent within two minutes still gets its
  answer. A press has no time of its own, so queued presses are left alone: one lost to a
  restart is pressed again.
- **Buttons are `callback_query` updates**, only delivered when `allowed_updates` asks for
  them, with at most 64 bytes of `callback_data` (so the verb is one letter and a search id
  is 8 characters), and each one is closed with `answerCallbackQuery` or the phone spins
  until it expires. Keyboards are redrawn in place with `editMessageReplyMarkup`.

## Commands

```bash
wallapop serve           # the bot, the watcher, the daily pass, and /healthz on :8000
wallapop watch --dry-run # one round of everybody's searches, printing what would be announced
wallapop watch           # one round, announcing on Telegram (--deep, --chat <id>)
wallapop searches        # every chat and its searches, each as an address
wallapop searches add [--chat <id>] [--name <name>] '<address>'
wallapop run --dry-run   # list what would be reactivated, touch nothing
wallapop run             # one catalogue pass
wallapop session import --cookie '<value>'   # store the browser session cookie
wallapop session show    # what is stored and whether it can renew itself
wallapop session refresh # renew now, which is how you check it works
```

## Configuration

The bot and the watcher:

| Variable | Default | What it does |
| :--- | :--- | :--- |
| `WALLA_TELEGRAM_TOKEN` | – | Bot token. Empty means the watcher runs and announces nothing |
| `WALLA_TELEGRAM_CHAT` | – | The owner's chat, always a user of the bot |
| `WALLA_MAX_USERS` | `20` | Chats that may join |
| `WALLA_MAX_SEARCHES` | `3` | Searches one chat may keep |
| `WALLA_WATCH_MIN` / `WALLA_WATCH_MAX` | `5m` / `15m` | The random wait between rounds |
| `WALLA_WATCH_MIN_PAUSE` / `WALLA_WATCH_MAX_PAUSE` | `3s` / `15s` | Random pause between the requests of a round |
| `WALLA_SEARCH_PAGES` | `3` | Pages of each search read on a deep round. One page is 40 listings |
| `WALLA_DEEP_EVERY` | `1h` | How often a round reads every page instead of the first one |
| `WALLA_WATCH_MAX_AGE` | `24h` | A listing older than this is recorded without a message |
| `WALLA_WATCH_MAX_ALERTS` | `10` | Messages per chat per round. The rest are counted in one line |
| `WALLA_WATCH_PHOTOS` | `2` | Thumbnails hashed per new listing |
| `WALLA_WATCH_DROP` | `5` | Percent a listing has to shed, against its own lowest price, before the fall is announced. `0` says nothing about prices |
| `WALLA_SEEN_TTL` | `720h` | How long a listing is remembered |

The catalogue:

| Variable | Default | What it does |
| :--- | :--- | :--- |
| `WALLA_INTERVAL` | `24h` | Time between catalogue passes. Under a minute is refused |
| `WALLA_RETRY_EVERY` | `15m` | How soon a failed pass is tried again. Under a minute is refused |
| `WALLA_MIN_PAUSE` / `WALLA_MAX_PAUSE` | `20s` / `90s` | Random pause between listings when reactivating |
| `WALLA_MAX_PER_RUN` | `25` | Ceiling on listings touched in one pass |
| `WALLA_WARN_BEFORE` | `72h` | How long before the cookie expires `/healthz` turns to `warn` |

General:

| Variable | Default | What it does |
| :--- | :--- | :--- |
| `WALLA_DATA_DIR` | `./data` | Where everything in [Data](#data) is kept |
| `WALLA_PORT` | `8000` | Port for `/healthz` |
| `WALLA_PUSHGATEWAY` | – | Pushgateway base URL. Empty means report nothing, which is right off the Pi |
| `WALLA_LOG_JSON` | – | `1` for JSON logs, which is what Vector collects |

The endpoints, so a change on their side is a redeploy and not a rebuild:

| Variable | Default |
| :--- | :--- |
| `WALLA_BASE_URL` | `https://api.wallapop.com` |
| `WALLA_WEB_URL` | `https://es.wallapop.com` |
| `WALLA_PATH_SEARCH` | `/api/v3/search` |
| `WALLA_PATH_ITEMS` | `/api/v3/user/items` |
| `WALLA_PATH_REACTIVATE` | `/api/v3/items/%s/reactivate` |
| `WALLA_REACTIVATE_METHOD` | `PUT` |
| `WALLA_SIGN_SCHEME` | `none`, or `pipe` / `legacy` if they ever sign requests again |
| `WALLA_DEVICE_ID` | from the `device_id` claim |
| `WALLA_APP_VERSION` | `826680` |

## Health

`/healthz` answers **200 whenever the process is alive**, and the body carries the state:

```json
{
  "status": "ok",
  "session": "ok",
  "renewable_days_left": 27.4,
  "next_run": "2026-09-24T15:25:02+02:00",
  "last_run": { "catalogue": 7, "expired": 0, "reactivated": [] },
  "next_watch": "2026-09-23T16:43:52+02:00",
  "last_watch": { "users": 2, "requests": 1, "watched": 1, "scanned": 40, "duplicates": 0, "new": [] }
}
```

`status` is `down` when there is no session or the cookie has expired, `warn` when it ends
within `WALLA_WARN_BEFORE` or the last pass failed, and `ok` otherwise. A failed round of
searches does not change it: the next round is minutes away.

Both jobs report as gauges to the Pushgateway, and the alert rules decide what is worth
waking somebody for:

| Job | Gauges |
| :--- | :--- |
| Catalogue | `wallapop_last_run_status`, `wallapop_last_run_timestamp`, `wallapop_session_days_remaining`, `wallapop_expired_listings`, `wallapop_reactivated_listings` |
| Watcher | `wallapop_watch_status`, `wallapop_watch_timestamp`, `wallapop_watch_users`, `wallapop_watch_searches`, `wallapop_watch_requests`, `wallapop_watch_shared`, `wallapop_watch_scanned`, `wallapop_watch_new`, `wallapop_watch_duplicates`, `wallapop_watch_cheaper` |

Telegram carries the listings, and the answers to whoever asked. The state of the service
is a metric.
