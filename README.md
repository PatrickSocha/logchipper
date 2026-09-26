# LogChipper

A self-hosted log collector and viewer. SQLite backend, SSE live stream, filterable UI.

## Quick Start

```bash
docker compose up --build
```

Open http://localhost:7070

## Configuration

Each setting can be set as an env var (Dockerfile / `docker-compose.yml`) or a command-line flag.

| Env var          | Flag             | Default                      | Description                                                   |
|------------------|------------------|------------------------------|---------------------------------------------------------------|
| `PORT`           | `-port`          | `7070` (Docker) / `8080`     | HTTP listen port                                              |
| `DB_PATH`        | `-db`            | `/data/logchipper.db` (Docker) | SQLite file path                                              |
| `RETENTION_DAYS` | `-retention`     | `30`                         | Delete events older than N days                               |
| `SYSLOG_ENABLE`  | `-syslog-enable` | `true`                       | Enable the syslog listener                                    |
| `SYSLOG_ADDR`    | `-syslog`        | `:514`                       | Syslog listen address (UDP and TCP)                           |
| `AUTH`           | `-auth`          | `false`                      | Require a login (see below)                                   |
| `ACCESS_MODE`    | `-access`        | `network`                    | Who can connect: `local`, `network` or `internet` (see below) |
| `ALLOWED_IPS`    | `-allowed-ips`   | empty                        | Comma-separated IPs/CIDRs, e.g. `203.0.113.7,192.168.1.0/24`  |
| `INGEST_TOKEN`   | `-ingest-token`  | empty                        | If set, `POST /api/logs` and `/api/logs/text` need `Authorization: Bearer <token>` |
| `MAX_BODY_BYTES` | `-max-body`      | `1048576` (1 MiB)            | Max HTTP request body; larger gets `413`                      |
| `MAX_QUERY_LIMIT`| `-max-query-limit` | `1000`                     | Max events per `GET /api/logs` (`limit` is clamped)           |
| `MAX_STREAM_CLIENTS` | `-max-stream-clients` | `100`              | Max concurrent `/api/stream` connections; extra get `503`     |
| `SYSLOG_MAX_MESSAGE_BYTES` | `-syslog-max-message` | `8192`       | Max syslog message. UDP is truncated; a longer TCP line closes the connection |
| `SYSLOG_MAX_CONNS` | `-syslog-max-conns` | `256`                    | Max concurrent syslog TCP connections                         |
| `HTTP_READ_TIMEOUT` | `-read-timeout` | `30s`                      | Max time to read a full HTTP request (header timeout is 10s)  |

On startup the server logs the URL to open, e.g. `open http://192.168.1.20:7070`.

### Access control

`ACCESS_MODE` applies to both HTTP and syslog (UDP/TCP):

- `local`: loopback only.
- `network`: loopback plus private/LAN ranges (10/8, 172.16/12, 192.168/16, link-local, CGNAT, IPv6 ULA).
- `internet`: anyone.

`ALLOWED_IPS` adds extra IPs/CIDRs on top of `local` or `network`. With `internet`, setting it restricts access to only those addresses.

Client IPs are taken from the socket address; `X-Forwarded-For` is ignored, so behind a reverse proxy every request appears to come from the proxy. Under Docker Desktop, published-port traffic often appears from the bridge gateway (172.x), which `local` will block.

### Authentication

Set `AUTH=true` to require a login for the UI and `/api/*` (session cookie via `POST /api/login`). `/healthz` is always open.

On first start there is no account: the first visit to the UI asks you to create one (username and a password of at least 8 characters). It is stored in the `users` table with the password hashed (PBKDF2-SHA256). Only one account is supported, and once it exists the setup form is gone. Create it straight after enabling auth, especially with `ACCESS_MODE=internet`, since until then anyone who can reach the UI could claim it.

To reset a forgotten password, stop the server and delete the account: `sqlite3 logchipper.db "DELETE FROM users"`. The next visit asks you to create it again.

### Syslog

Syslog (RFC 3164/5424 over UDP/TCP) has no authentication mechanism, so `AUTH` and `INGEST_TOKEN` do not cover it. Protect it at the network layer: run it inside a private network/VPC and set `ACCESS_MODE=network` or `ALLOWED_IPS` (plus a firewall/security group), or set `SYSLOG_ENABLE=false` and ship logs over HTTP with `INGEST_TOKEN`. Never expose it to the internet. UDP source addresses are spoofable, so treat the allowlist on UDP as a filter, not a guarantee.

With syslog enabled, RFC 3164 and ISO 8601 style messages are accepted on UDP and TCP (newline-delimited) at `SYSLOG_ADDR`, e.g. `logger -n localhost -P 514 "hello"`.

### Database migrations

Schema changes live in `internal/db/migrations/` as numbered SQL files (`0001_...sql`, `0002_...sql`). They are embedded in the binary and applied in filename order at startup, once each, tracked in the `schema_migrations` table. To change the schema, add the next numbered file; never edit one that has shipped.

## API

### Ingest (JSON)

```
POST /api/logs
Content-Type: application/json

{
  "source":  "my-app",
  "level":   "error",
  "message": "something went wrong",
  "meta":    "optional extra string"
}
```

Levels: `debug`, `info`, `notice`, `warn`, `error`

`created_at` is optional (RFC3339); defaults to server time.

### Ingest (plain text)

```
POST /api/logs/text
X-Source: my-app
X-Level: warn

plain text message body
```

### Query

```
GET /api/logs?q=keyword&level=error&source=my-app&limit=100&after_id=500
```

### Sources list

```
GET /api/sources
```

### Live stream (SSE)

```
GET /api/stream
```

Returns `text/event-stream`. Each event is a JSON-encoded log entry.

## Sending logs from your app

**curl:**
```bash
curl -s -X POST http://localhost:7070/api/logs \
  -H 'Content-Type: application/json' \
  -d '{"source":"deploy","level":"info","message":"Deploy started"}'
```

**From shell scripts:**
```bash
log() {
  curl -s -X POST http://logchipper:7070/api/logs/text \
    -H "X-Source: $(hostname)" \
    -H "X-Level: info" \
    --data-binary "$*" > /dev/null
}

log "Server started on port 3000"
```

## Purge schedule

Automatic purge runs every hour. Events older than `RETENTION_DAYS` are deleted.

## License

Copyright (C) 2026 Patrick Socha

Licensed under the [GNU Affero General Public License v3.0](LICENSE).
