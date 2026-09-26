# LogChipper

A self-hosted log collector and viewer. Send logs over syslog or HTTP, then search and live-tail them in the browser. Single binary, SQLite storage.

## Quick start

```bash
docker compose up --build
```

Open http://localhost:7070.

## Sending logs

**Syslog** (UDP or TCP, port 514):

```bash
logger -n localhost -P 514 "hello"
```

**HTTP:**

```bash
curl -X POST http://localhost:7070/api/logs \
  -H 'Content-Type: application/json' \
  -d '{"source":"my-app","level":"info","message":"hello"}'
```

Levels: `debug`, `info`, `notice`, `warn`, `error`. For plain text, `POST /api/logs/text` with `X-Source` and `X-Level` headers.

## Configuration

Set via environment variables (see `docker-compose.yml`) or flags (`logchipper -h`).

| Variable         | Default   | Description                                                     |
|------------------|-----------|-----------------------------------------------------------------|
| `AUTH`           | `true`    | Require a login. The first visit asks you to create the account |
| `ACCESS_MODE`    | `network` | Who can connect: `local`, `network` (LAN) or `internet`         |
| `ALLOWED_IPS`    |           | Extra IPs/CIDRs to allow. With `internet`, *only* these         |
| `INGEST_TOKEN`   |           | Require `Authorization: Bearer <token>` for HTTP ingest         |
| `RETENTION_DAYS` | `30`      | Delete logs older than this                                     |
| `SYSLOG_ENABLE`  | `true`    | Listen for syslog                                               |
| `PORT`           | `7070`    | HTTP UI port                                                       |

Size and connection limits are also configurable; run `logchipper -h` for the full list.

## Security

- Syslog has no authentication. Keep it on a private network, or set `SYSLOG_ENABLE=false` and use HTTP with `INGEST_TOKEN`.
- Create the account as soon as you first start it: until then, anyone who can reach the UI can claim it. Set `AUTH=false` to turn login off.
- Forgot the password? Stop the server, run `sqlite3 logchipper.db "DELETE FROM users"`, and set it up again.

## License

Copyright (C) 2026 Patrick Socha. Licensed under the [AGPL-3.0](LICENSE).
