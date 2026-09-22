# Deploy

Built from this repo by [rpi-services](https://github.com/PlatanosVerdes/rpi-services),
pinned to the CalVer tag that `auto-tag.yml` creates on every push to main.

Entry for its `docker-compose.yml`:

```yaml
  # --- Wallapop: reactivar anuncios caducados y vigilar las busquedas guardadas ---
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
      # State is a metric; Telegram carries only the listings that have just appeared.
      WALLA_PUSHGATEWAY: http://pushgateway:9091
      WALLA_TELEGRAM_TOKEN: ${WALLA_TELEGRAM_TOKEN}
      WALLA_TELEGRAM_CHAT: ${WALLA_TELEGRAM_CHAT}
    volumes:
      # The session lives here, and so does what has already been seen.
      - ${APP_CONFIG_PATH}/wallapop-manager:/data
```

The memory limit goes up from 64m because a round decodes thumbnails to hash them.

The session is the only manual step, and only when the cookie finally expires:

```bash
docker exec wallapop-manager wallapop session import --cookie '<the __Secure-next-auth.session-token value>'
docker exec wallapop-manager wallapop session show
```

The import renews once before reporting success, so it doubles as the check.

What is being watched, and a round without waiting for the clock:

```bash
docker exec wallapop-manager wallapop searches
docker exec wallapop-manager wallapop watch --dry-run
docker exec wallapop-manager wallapop run --dry-run
```

Searches are added, removed and switched on or off in the Wallapop app. Nothing here
writes to the account.
