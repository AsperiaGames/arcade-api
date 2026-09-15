# Arcade API

Arcade API is the backend service responsible for the arcade points ledger. It stores account balances, lifetime totals, transaction receipts, and hold-based spend flows in PostgreSQL, while enforcing the business rules that protect the ledger from accidental overdraws and runaway earnings.

## Overview

This repository is organized around a small set of durable primitives:

- Accounts: per-user balance and lifetime counters
- Receipts: append-only audit log of all earn/spend/purchase activity
- Holds: short-lived reserved debits for operations that must be committed later
- Earn windows: daily limits per user for earned points
- Guest earnings: anonymous earnings that are later claimed during sign-up

The core logic lives in the Go packages under `internal/` and is designed to be safe under concurrency and consistent with the database's constraints.

## Key behavior

- Balance is never allowed to go negative
- Earn credits are capped by a daily window
- Spend operations are atomic and use a database precondition so concurrent debits cannot overdraw the same account
- Receipts are written in the same transaction as balance mutations for traceability
- Holds expire automatically and can be swept or released by higher-level workflow code

## Project structure

- `cmd/server/` - HTTP server entrypoint (currently scaffolded for the app runtime)
- `internal/config/` - environment validation and startup configuration
- `internal/ledger/` - points ledger logic and immutable receipt records
- `internal/store/` - database pool creation and migration runner
- `internal/auth/` - authentication-related integration code
- `internal/httpapi/` - HTTP layer and request handlers
- `internal/arcade/` - arcade/domain logic
- `migrations/` - Goose SQL migrations

## Requirements

- Go 1.27.1 or compatible Go toolchain
- PostgreSQL 16+
- Docker (recommended for local integration tests via Testcontainers)

## Configuration

The service reads settings from environment variables through `internal/config.Load()`.

| Variable | Required | Description |
| --- | --- | --- |
| `PORT` | No | HTTP port for the service. Default: `8080` |
| `DATABASE_URL` | Yes | Postgres connection string |
| `STORM_GATE_URL` | Yes | Storm-Gate OIDC issuer URL |
| `SERVICE_TOKEN` | Yes | Shared secret for trusted server-to-server calls |
| `CORS_ORIGINS` | No | Comma-separated allowed browser origins |
| `POINTS_MAX_SINGLE_EARN` | No | Maximum single earn event. Default: `10000` |
| `POINTS_MAX_DAILY_EARN` | No | Daily earning cap per user. Default: `10000` |
| `POINTS_MAX_OFFLINE_CLAIM` | No | Max offline claim amount. Default: `50000` |
| `HOLD_TTL_SECONDS` | No | Time before held debits expire. Default: `120` |

Example:

```bash
export PORT=8080
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/arcade"
export STORM_GATE_URL="https://storm-gate.example.com"
export SERVICE_TOKEN="replace-with-a-long-random-secret-at-least-32-chars"
export CORS_ORIGINS="https://arcade.example.com,https://admin.example.com"
export POINTS_MAX_SINGLE_EARN=10000
export POINTS_MAX_DAILY_EARN=10000
export POINTS_MAX_OFFLINE_CLAIM=50000
export HOLD_TTL_SECONDS=120
```

## Local setup

1. Copy the example environment file and adjust values for your machine:

```bash
cp .env.example .env
```

2. Start PostgreSQL locally or via Docker:

```bash
docker run --name arcade-postgres \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=arcade \
  -p 5432:5432 \
  -d postgres:16-alpine
```

3. Load your environment variables and run the app bootstrap that invokes the migration step before serving traffic.

The `store.Migrate` function applies the Goose migrations in `migrations/` and is intended to run at startup for the deployed service. This repository currently exposes the ledger and storage packages, and the migration flow is validated through the Go test suite and the migration tooling rather than a standalone `go run` entrypoint.

## Migrations

Database schema changes are managed with Goose and live under `migrations/`.

```bash
go test ./...
```

The migration layer is intentionally separate from the business logic so the ledger code remains focused on state transitions and invariants.

## Testing

The repository already includes real integration tests for the ledger behavior using PostgreSQL in Docker via Testcontainers.

```bash
go test ./...
```

Verified working state at the time of writing:

- `ok github.com/HoseaCodes/arcade-api/internal/ledger 6.842s`

## HTTP API

Player routes take a Storm-Gate RS256 JWT, verified locally against the published
JWKS — there is no shared signing secret. Errors are always `{"msg": "..."}`:
blog-portfolio's React app and 13 arcade game builds parse these exact shapes, and
none of them can be redeployed on this service's schedule.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/health` | Open. Reports `version` and `commit`. |
| `GET` | `/api/points/balance` | |
| `GET` | `/api/points/transactions?limit=` | Default 50, capped at 200 |
| `POST` | `/api/points/earn` | `429` once the daily budget is spent |
| `POST` | `/api/points/spend` | `402` carries `balance` and `required` |
| `POST` | `/api/points/sync` | One-shot offline claim; `409` if already used |
| `POST` | `/api/points/claim-guest` | Moves an anonymous balance onto the account |

Service routes take `SERVICE_TOKEN`, compared in constant time. A player JWT is
**never** accepted here — that boundary is all that stands between a browser and
arbitrary balance mutation.

| Method | Path | Notes |
|---|---|---|
| `POST` | `/internal/credit` | Requires `idempotencyKey`; replays return `applied: false` |
| `POST` | `/internal/holds` | Debits up front |
| `POST` | `/internal/holds/{id}/commit` | Writes the receipt |
| `POST` | `/internal/holds/{id}/release` | Returns the points |

### Why 429 on the daily cap

Not 400. The arcade client already maps that status to `daily-cap` and stops
retrying, where a 400 reads as a malformed request and gets retried. Choosing the
status the existing client already understands is what let the cap ship without
redeploying 13 game builds.

### Why holds rather than spend-then-refund

blog-portfolio still owns purchases in MongoDB, so a debit here and a purchase row
there cannot share a transaction. Spend-then-refund breaks precisely when the
*refund* is the call that fails — the player is left short with no recovery path
from the caller's side. An expiring hold heals with nobody online.

## Deployment

Fly.io, matching the house pattern used by blog-portfolio: port 8080, forced HTTPS,
scale to zero. `make deploy` builds remotely and ships; `make status` reads `/health`
to show what is actually running.

### Releases

Deploying is normally not a thing anyone does by hand. Every push to `main` runs
`.github/workflows/release.yml`, which runs the pull-request checks, tags a version,
publishes a GitHub release with notes, and deploys that tag to Fly.

The version comes from the commits since the last tag, read as
[conventional commits](https://www.conventionalcommits.org): a breaking change bumps
major, a `feat` bumps minor, anything else bumps patch. Below `1.0.0` a breaking
change bumps minor instead — a stray `!` should not declare the API stable. Preview
what the next push would publish:

```bash
make release-preview
```

`workflow_dispatch` on the same workflow takes a `bump` override (this is how
`1.0.0` gets cut) and a `deploy` toggle for tagging without shipping.

The deploy links the tag into the binary and then polls `/health` until it reports
that version. A green `flyctl deploy` only means the machines started; this is what
proves the release just tagged is the one serving traffic.

One secret is required: **`FLY_API_TOKEN`**, a repository secret holding the output
of `fly tokens create deploy -a ac-arcade-api`.

Postgres is **Neon** rather than Fly Postgres — managed instead of self-operated,
and its database branching lets a migration be rehearsed against a copy of
production and then discarded.

Use Neon's **direct** endpoint, not the pooled one. The pooler runs PgBouncer in
transaction mode, which breaks pgx's default extended protocol with prepared
statements, and long-lived Fly machines already pool through pgxpool.

One thing to watch: `min_machines_running = 0` stacks with Neon's autosuspend, so
the first request after an idle period wakes both. Fine for earn — the arcade client
is asynchronous with a localStorage fallback — but noticeable on a balance read.

## Notes

This repo owns the points ledger and the game-side surface. Everything that *uses*
points — products, purchases, the redeem store, AI-art, PayPal — stays in
blog-portfolio and reaches the balance through `/internal`.

## License

This project does not currently declare a license file in the repository. Add a license before public distribution or external deployment.
