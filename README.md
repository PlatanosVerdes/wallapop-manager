# wallapop-manager

Looks after my Wallapop account. Two jobs, one binary:

- **The catalogue.** A listing expires after a while and goes quiet until somebody presses
  **Reactivar** on it. This does that pass once a day, and says nothing unless something
  needs a human.
- **The searches.** The saved searches kept in the app are replayed every few minutes, and
  anything genuinely new in them arrives on Telegram with its photo and a link.

Runs on the Raspberry from [rpi-services](https://github.com/PlatanosVerdes/rpi-services),
same conventions as the rest: Go with no dependencies, CalVer tag on every push to main,
`/healthz` for the blackbox probe.

## The API

Wallapop's [Connect API](https://developers.wallapop.com) is open to PRO sellers and
integrators with a registered OAuth client, and documents none of this, so it uses the same
private API the web app does. Four endpoints, all captured from the browser:

| Call | Session | What for |
| :--- | :--- | :--- |
| `GET /api/v3/user/items` | yes | The catalogue. The pagination cursor comes back in the `X-NextPage` response header, not in the body |
| `PUT /api/v3/items/{id}/reactivate` | yes | Presses the button. Answers `204 No Content` |
| `GET /api/v3/searchalerts/savedsearch` | yes | The saved searches, each with the query the app stored and whether its alert is on |
| `GET /api/v3/search` | **no** | The catalogue search, which is how a saved search is replayed |

Requests carry the bearer plus the device headers a browser sends (`x-deviceid`,
`x-appversion`, `deviceos`). There is no request signing: the older `X-Signature` scheme
(HMAC-SHA256 over method, path and timestamp) is gone from the web app, and lives on here
behind `WALLA_SIGN_SCHEME` for the day it returns.

The search endpoint answers the same to anybody, so the watcher sends **no bearer and no
device id** on it. Only the list of saved searches needs the session, and it is read once
per round. Nothing in here ever writes to the account: no search is created, enabled or
disabled, ever. The switch in the app is the setting.

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
day, a random 20-90s pause between listings, and a watcher that does not identify itself.

## Telling a new listing from the same one again

The searches are for vehicles, and vehicles are advertised by dealer networks: one van is
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

Measured on a live round of the saved searches: of 5.886 pairs of listings, 1.5% sat under
10 bits apart and everything else piled up above 20, so the threshold sits in an empty gap.
A thumbnail costs about 25 ms including the download, and only listings never seen before
are hashed, so a quiet round downloads nothing at all.

What has been seen lives in `data/seen.json` and is forgotten after `WALLA_SEEN_TTL`.
The first round of a new search announces nothing: its whole first page was already there.

## The bot

It announces without being asked, and answers four things when it is:

| Command | What it says |
| :--- | :--- |
| `/wp_searches` | The searches being watched, each with a switch: pressing it silences that search, pressing it again gives it back. The ones switched off in the app are counted, not listed: the terminal command prints those, with their queries |
| `/wp_status` | The last round, the next one, and how long the session has left |
| `/wp_check` | Runs a round now. Answers `ya hay una ronda en marcha` rather than queueing behind one |
| `/wp_help` | The list above, built from the same table the bot dispatches from |

Every announced listing carries two buttons, **open the listing** and **silence this
search**, because the moment it is clear that a search is talking too much is the moment
one of its messages arrives. The address rides on the button rather than in the text.

Telegram draws messages in a proportional font, so columns padded with spaces only line up
inside `<pre>`. The grid of watched searches is a `<pre>` block and everything else leans
on `<b>` and `<i>`; the long listing with each stored query stays on the terminal, which is
the only place it is readable.

Silence is this service's own state, kept in `data/mutes.json`. It never writes to
Wallapop: the alert switch in the app stays where its owner left it, and a silenced search
is simply one this has nothing to say about for now. So the app can have a search on while
the bot is quiet about it, and turning it back on here changes nothing over there.

A command sent while the container is being replaced is not lost: the queue handed over on
the first read is judged by the age of each message, and anything sent within two minutes
still gets its answer. A press has no time of its own, so the queue's presses are left
alone instead: one lost to a restart is pressed again, and the button shows which way it
went. The log carries the lag of every command, because a bot that feels slow is usually a
command that arrived during a deploy.

A button press is a `callback_query`, which is only delivered when `allowed_updates` asks
for it, carries at most 64 bytes of `callback_data` (a saved search id is a 36 character
uuid, so the verb in front of it has to be short), and must be answered with
`answerCallbackQuery` or the phone spins until the press expires. The keyboard is then
redrawn in place with `editMessageReplyMarkup`, so the switch shows the position it was
just moved to.

Two rules come from the bot being shared with the other small services on the Pi:

- **One process per bot may read.** Telegram hands each update to whoever asks first and
  answers a second reader with a 409, so a token has exactly one owner of `getUpdates`.
  Any other service using this bot may only send. The day a second one wants commands,
  that owner becomes a router and this service exposes its commands over HTTP instead.
- **Names carry the `wp_` prefix**, because the command menu belongs to the bot and not to
  a service. A command that is not ours is somebody else's and gets no answer, and no
  reply either way goes anywhere but `WALLA_TELEGRAM_CHAT`: a bot is public, and anybody
  who finds it can write to it.

## Commands

```bash
wallapop run --dry-run   # list what would be reactivated, touch nothing
wallapop run             # one catalogue pass
wallapop watch --dry-run # one round of the searches, printing what would be announced
wallapop watch           # one round, announcing on Telegram
wallapop searches        # the saved searches, and which of them are watched
wallapop serve           # daily pass, the watcher on a random clock, and /healthz on :8000
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
| `WALLA_DATA_DIR` | `./data` | Where the session, the last runs and what has been seen are kept |
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
| `WALLA_TELEGRAM_CHAT` | – | Chat the listings are sent to, and the only one the bot obeys |
| `WALLA_WATCH_MIN` / `WALLA_WATCH_MAX` | `5m` / `15m` | The round is run after a random wait in this range, so the pattern is not a metronome |
| `WALLA_WATCH_ALL` | – | `1` watches every saved search instead of only the ones whose alert is on in the app |
| `WALLA_WATCH_MAX_AGE` | `24h` | A listing older than this is recorded without a message: it was already there |
| `WALLA_WATCH_MAX_ALERTS` | `10` | Messages per round. The rest are counted in one line at the end |
| `WALLA_WATCH_PHOTOS` | `2` | Thumbnails hashed per new listing |
| `WALLA_WATCH_MIN_PAUSE` / `WALLA_WATCH_MAX_PAUSE` | `3s` / `15s` | Random pause between searches within a round |
| `WALLA_SEEN_TTL` | `720h` | How long a listing is remembered for the duplicate check |

The endpoints, so a change on their side is a redeploy and not a rebuild:

| Variable | Default |
| :--- | :--- |
| `WALLA_PATH_ITEMS` | `/api/v3/user/items` |
| `WALLA_PATH_REACTIVATE` | `/api/v3/items/%s/reactivate` |
| `WALLA_REACTIVATE_METHOD` | `PUT` |
| `WALLA_PATH_SAVED_SEARCHES` | `/api/v3/searchalerts/savedsearch` |
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
  "last_watch": { "watched": 3, "scanned": 45, "duplicates": 21, "new": [] }
}
```

`status` is `down` when there is no session or the cookie has expired, `warn` when it ends
within `WALLA_WARN_BEFORE` or the last pass failed, and `ok` otherwise. A failed round of
searches does not change it: the next round is minutes away.

Both jobs report as gauges, and the alert rules decide what is worth waking somebody for:
`wallapop_last_run_status`, `wallapop_session_days_remaining`, `wallapop_expired_listings`,
`wallapop_reactivated_listings`, and for the watcher `wallapop_watch_status`,
`wallapop_watch_searches`, `wallapop_watch_scanned`, `wallapop_watch_new`,
`wallapop_watch_duplicates`.

Telegram carries one thing only: a listing that has just appeared in a watched search.
