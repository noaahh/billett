# billett

`billett` turns SBB receipt emails into a personal journey log. It is an expense tracker for Swiss public transport and, at the same time, a way to reflect on your travels: every ticket becomes a line in a ledger, a route on a map, and part of your spend, distance, and time totals.

The process is meant to be automatic and straightforward. Receipts arrive through email forwarding or a one-time import of your mail archive, parsing and route reconstruction happen in the background, and the dashboard answers at a glance what your travel cost and where it took you.

Everything runs self-hosted as one Go process with an embedded browser interface and a SQLite database. The repository includes a secured container deployment, automatic HTTPS through Caddy, a Cloudflare Email Worker adapter, historical MBOX import, data correction, verified backup and restore, and portable exports.

## What it does

- Parses German EasyRide receipts and route-based tickets from `Ihr Online-Kauf bei der SBB` emails.
- Imports individual `.eml` files, Gmail Takeout MBOX archives, browser uploads, and authenticated inbound email deliveries.
- Records every ingestion attempt with its parser version and outcome.
- Deduplicates messages and individual tickets without losing the ingestion history.
- Separates observed EasyRide journeys from purchased tickets whose actual use is unconfirmed.
- Reconstructs route geometry in a background queue and records provider provenance.
- Supports correcting, excluding, rerouting, and deleting journeys in the dashboard.
- Exports journey data as JSON, CSV, or GeoJSON.
- Creates online SQLite backups and verifies backups before restore.

## Data meaning

An EasyRide receipt is treated as evidence of an observed journey. A normal ticket purchase is stored as an unconfirmed ticket until the owner confirms it in the dashboard. Ticket validity dates do not count as actual travel times.

| Field | Values | Meaning |
| --- | --- | --- |
| `evidenceType` | `observed`, `ticket` | EasyRide records an observed ride. A normal purchase records a ticket. |
| `occurrenceStatus` | `confirmed`, `unconfirmed`, `excluded` | Controls whether journey analytics include the record. |
| `timeSource` | `recorded`, `validity_window`, `user_supplied`, `timetable_estimate` | States what the validity timestamps represent. |

Dashboard journey, distance, duration, mode, and map metrics include only confirmed journeys. Spend includes purchased tickets, including unconfirmed tickets.

Route reconstruction is analytical metadata, not proof that a journey occurred. When an old timetable date is unavailable, `billett` reuses an existing local route for the same corridor or uses current timetable topology. These results are marked as estimated and retain a route data version.

## Start with Docker Compose

```sh
git clone https://github.com/noaahh/billett.git
cd billett
cp .env.example .env
openssl rand -base64 32
```

Put the generated password in `BILLETT_AUTH_PASSWORD` inside `.env`, then start the service:

```sh
docker compose up -d
docker compose ps
```

Open `http://127.0.0.1:8080` and sign in with the configured credentials. Compose pulls `ghcr.io/noaahh/billett:latest` when available and can build the image from the checkout when it is not.

To expose the service with automatic HTTPS, point the configured domain to the host, set `BILLETT_DOMAIN` in `.env`, and enable the public profile:

```sh
docker compose --profile public up -d
```

The public deployment requires `BILLETT_AUTH_USERNAME` and `BILLETT_AUTH_PASSWORD`. The application also refuses a non-loopback listen address when authentication is absent. Do not expose Basic authentication over plain HTTP.

## Operate

`GET /api/health` is intentionally unauthenticated so container and external health checks can use it. It reports the application version, SQLite schema version, database reachability, and whether authentication and inbound email are configured.

Back up while the service is online, copy the result off the host, and verify it:

```sh
docker compose exec -T billett \
  billett backup --output /data/backups/billett-$(date -u +%Y%m%dT%H%M%SZ).sqlite
docker compose cp billett:/data/backups/billett-20260808T120000Z.sqlite ./backups/
billett verify-database ./backups/billett-20260808T120000Z.sqlite
```

Restore with the service stopped. The current database is preserved as a verified `.before-restore-*` copy before being replaced:

```sh
docker compose stop billett
docker compose cp ./backups/billett-20260808T120000Z.sqlite billett:/data/restore.sqlite
docker compose run --rm billett restore --from /data/restore.sqlite --replace
docker compose up -d billett
```

Schema migrations apply automatically on startup and only move forward, so create a verified backup before upgrading. Raw source emails are not retained unless `BILLETT_RAW_ARCHIVE_DIR` is set; when enabled, that directory contains personal data and belongs under the same backup, access, and deletion policy as the database.

## Run from source

Go `1.26` is required.

```sh
BILLETT_AUTH_USERNAME=owner \
BILLETT_AUTH_PASSWORD='replace-this' \
go run ./cmd/billett serve
```

The default address is `127.0.0.1:8080` and the default database is `./data/billett.sqlite`.

## Import existing history

Individual saved messages can be imported together:

```sh
billett import receipt-1.eml receipt-2.eml
```

For Gmail history, request a Mail export from [Google Takeout](https://support.google.com/accounts/answer/3024190), extract the archive, then import its MBOX file:

```sh
billett import-mbox --report import-report.json Takeout/Mail/All\ mail.mbox
```

The importer continues past unsupported messages, reports duplicates separately, and writes an optional machine-readable report. Run the same import again safely if a batch is interrupted.

## Automatic email delivery

`deploy/cloudflare-email-worker` contains an adapter for [Cloudflare Email Routing](https://developers.cloudflare.com/email-routing/). It accepts only the configured recipient and supported SBB subjects, then forwards the complete RFC 5322 message to `POST /api/inbound-email` with a bearer token.

Generate a separate inbound secret:

```sh
openssl rand -hex 32
```

Set it as `INBOUND_TOKEN` in `billett` and as the Worker secret `BILLETT_INBOUND_TOKEN`. Copy `deploy/cloudflare-email-worker/wrangler.toml.example` to `wrangler.toml`, configure the HTTPS inbound URL, recipient, and `ALLOWED_FROM_DOMAINS`, then deploy:

```sh
cd deploy/cloudflare-email-worker
npx wrangler secret put BILLETT_INBOUND_TOKEN
npx wrangler deploy
```

For Gmail, configure forwarding filters for `subject:"EasyRide Kaufquittung"` and `subject:"Ihr Online-Kauf bei der SBB"`. Forwarding handles new mail. Use the MBOX importer for existing history.

The Worker defaults to accepting receipt subjects only when the envelope sender is `sbb.ch` or one of its subdomains. This is an ingestion filter, not cryptographic sender authentication. Keep email-provider spam and DMARC checks enabled.

## Commands

```text
billett serve
billett import receipt.eml [...]
billett import-mbox --report report.json mail.mbox
billett backup --output backup.sqlite
billett restore --from backup.sqlite --replace
billett export --format json|csv|geojson --output FILE
billett verify-database file.sqlite
billett smoke-route
billett seed-demo
billett version
```

Every database command accepts `-db`. The `BILLETT_DB` environment variable provides the same setting.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `BILLETT_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `BILLETT_DB` | `./data/billett.sqlite` | SQLite database path |
| `BILLETT_AUTH_USERNAME` | empty | Dashboard and management API user |
| `BILLETT_AUTH_PASSWORD` | empty | Dashboard and management API password |
| `BILLETT_RAW_ARCHIVE_DIR` | empty | Optional private raw email archive |
| `INBOUND_TOKEN` | empty | Bearer token for inbound email delivery |
| `GEOPS_API_KEY` | empty | Optional personal geOps routing key |
| `OSRM_URL` | public OSRM demo endpoint | Road geometry service |
| `OVERPASS_URL` | `https://overpass.osm.ch/api/interpreter` | Rail geometry service |
| `BILLETT_DOMAIN` | `journeys.example.com` | Caddy hostname in the public Compose profile |
| `BILLETT_VERSION` | `latest` | Container image tag |

## External service boundary

The application, database, parser, and dashboard are self-hosted. Depending on configuration, email delivery and route reconstruction use Cloudflare, `transport.opendata.ch`, OSRM, Overpass, geOps, OpenStreetMap tiles, and the Leaflet assets served by `unpkg.com`.

The default OSRM endpoint is a public demonstration service. Point `OSRM_URL` and `OVERPASS_URL` at services you operate when you need a controlled availability or privacy boundary. Route matching sends the origin, destination, date, validity time, and scheduled stop coordinates to the configured providers.

## Development

```sh
go test ./...
go vet ./...
node --test deploy/cloudflare-email-worker/src/index.test.js
docker build --tag billett:dev .
```

Released under the [MIT License](LICENSE).
