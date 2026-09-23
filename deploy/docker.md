# Deploy

Built from this repo by [rpi-services](https://github.com/PlatanosVerdes/rpi-services),
pinned to the CalVer tag that `auto-tag.yml` creates on every push to main.

Entry for its `docker-compose.yml`:

```yaml
  # --- Wallapop: reactivate expired listings daily, and the searches bot ---
  wallapop-manager:
    build:
      context: https://github.com/PlatanosVerdes/wallapop-manager.git#${WALLAPOP_VERSION}
      args:
        VERSION: ${WALLAPOP_VERSION}
    container_name: wallapop-manager
    labels:
      prometheus.probe: "http://wallapop-manager:8000/healthz"
    restart: unless-stopped
    profiles: [wallapop, all]
    mem_limit: 128m
    memswap_limit: 128m
    dns:
      - 8.8.8.8
      - 1.1.1.1
    networks:
      - media-network
    environment:
      TZ: ${TZ:-Europe/Madrid}
      WALLA_PUSHGATEWAY: http://pushgateway:9091
      # A bot of its own: this service is the only reader of its updates.
      WALLA_TELEGRAM_TOKEN: ${WALLA_TELEGRAM_TOKEN}
      # The owner's chat, which keeps what was seen before the bot had users.
      WALLA_TELEGRAM_CHAT: ${WALLA_TELEGRAM_CHAT}
    volumes:
      # The session, the users and what has been seen for each of them.
      - ${APP_CONFIG_PATH}/wallapop-manager:/data
```

The memory limit goes up from 64m because a round decodes thumbnails to hash them.

The session is the only manual step, and only when the cookie finally expires:

```bash
docker exec wallapop-manager wallapop session import --cookie '<the __Secure-next-auth.session-token value>'
docker exec wallapop-manager wallapop session show
```

The import renews once before reporting success, so it doubles as the check.

Who uses the bot and what they look for, and a round without waiting for the clock:

```bash
docker exec wallapop-manager wallapop searches
docker exec wallapop-manager wallapop searches add --chat <id> '<address of a web search>'
docker exec wallapop-manager wallapop watch --dry-run
docker exec wallapop-manager wallapop run --dry-run
```

Searches live in `users.json` and are added, silenced and deleted from the bot. Nothing
in the watcher writes to, or even reads, the Wallapop account.
