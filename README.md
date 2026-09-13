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

## Notes

This repo is the data and accounting core behind the arcade rewards system. The surrounding HTTP authentication and service wiring are expected to be completed by the application entrypoint or deployment layer that consumes these packages.

## License

This project does not currently declare a license file in the repository. Add a license before public distribution or external deployment.
