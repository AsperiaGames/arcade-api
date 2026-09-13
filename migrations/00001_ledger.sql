-- +goose Up
-- +goose StatementBegin

-- The points ledger. Ported from blog-portfolio's Mongo collections, with the
-- two invariants that were previously enforced only by convention now enforced
-- by the database:
--
--   1. balance can never go negative          -> CHECK constraint
--   2. a credit can be applied at most once   -> UNIQUE idempotency_key
--
-- user_id is a Storm-Gate JWT subject, not a local key. Storm-Gate is a separate
-- service with its own database, so there is no foreign key to a users table and
-- there should not be one.
CREATE TABLE accounts (
    user_id                text        PRIMARY KEY,
    balance                bigint      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    lifetime_earned        bigint      NOT NULL DEFAULT 0,
    lifetime_spent         bigint      NOT NULL DEFAULT 0,
    lifetime_purchased     bigint      NOT NULL DEFAULT 0,
    claimed_offline        boolean     NOT NULL DEFAULT false,
    claimed_offline_amount bigint      NOT NULL DEFAULT 0,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

CREATE TYPE receipt_type AS ENUM ('earn', 'spend', 'purchase', 'sync', 'adjust');

-- Append-only audit log. Never updated, never deleted. `amount` is always
-- positive; the direction is implied by `type`. `balance_after` lets state be
-- reconstructed at any point without replaying the whole log.
CREATE TABLE receipts (
    id              bigserial    PRIMARY KEY,
    user_id         text         NOT NULL REFERENCES accounts (user_id),
    type            receipt_type NOT NULL,
    amount          bigint       NOT NULL CHECK (amount >= 0),
    balance_after   bigint       NOT NULL,
    meta            jsonb        NOT NULL DEFAULT '{}',
    -- Caller-supplied dedupe key, e.g. 'paypal:<orderId>'. NULL for operations
    -- with no external identifier; Postgres allows many NULLs under UNIQUE.
    idempotency_key text         UNIQUE,
    created_at      timestamptz  NOT NULL DEFAULT now()
);

CREATE INDEX receipts_user_created_idx ON receipts (user_id, created_at DESC);

-- Two-phase spend for work this service does not own.
--
-- blog-portfolio still owns products and purchases in MongoDB, so a debit here
-- and a purchase row there cannot share a transaction. A hold debits up front
-- and is then committed or released once the caller knows whether its own write
-- succeeded. Holds expire, so a caller that crashes mid-flow self-heals instead
-- of stranding the player's points.
CREATE TABLE holds (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    text        NOT NULL REFERENCES accounts (user_id),
    amount     bigint      NOT NULL CHECK (amount > 0),
    status     text        NOT NULL DEFAULT 'held'
                           CHECK (status IN ('held', 'committed', 'released', 'expired')),
    meta       jsonb       NOT NULL DEFAULT '{}',
    expires_at timestamptz NOT NULL,
    settled_at timestamptz,
    receipt_id bigint      REFERENCES receipts (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Only open holds are ever swept, so the index covers just those.
CREATE INDEX holds_open_expiry_idx ON holds (expires_at) WHERE status = 'held';
CREATE INDEX holds_user_idx ON holds (user_id);

-- Daily earn budget.
--
-- /points/earn accepts a client-supplied amount from a browser origin, and
-- Storm-Gate issues guest tokens to anyone unauthenticated, so an unbounded
-- earn endpoint is a minting hole. The per-call cap bounds one request; this
-- bounds a day. window_start is a UTC date.
--
-- Per-game budgets arrive with game sessions; this table intentionally matches
-- the per-user shape already shipped in Node so the cap does not regress at
-- cutover.
CREATE TABLE earn_windows (
    user_id      text   NOT NULL,
    window_start date   NOT NULL,
    earned       bigint NOT NULL DEFAULT 0 CHECK (earned >= 0),
    PRIMARY KEY (user_id, window_start)
);

-- Guest earnings are held apart from real accounts.
--
-- Storm-Gate mints a fresh guest identity per anonymous login, so writing guest
-- earns into `accounts` would create one unreachable balance per session from
-- an unauthenticated endpoint. Instead they accumulate here and are claimed once
-- at sign-up. The claimed_by/claimed_at stamp makes the claim idempotent and
-- unrepeatable; unclaimed rows expire and are pruned.
CREATE TABLE guest_earnings (
    guest_id   text        PRIMARY KEY,
    balance    bigint      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    claimed_by text        REFERENCES accounts (user_id),
    claimed_at timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT guest_claim_is_atomic
        CHECK ((claimed_by IS NULL) = (claimed_at IS NULL))
);

CREATE INDEX guest_earnings_unclaimed_idx ON guest_earnings (expires_at)
    WHERE claimed_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS guest_earnings;
DROP TABLE IF EXISTS earn_windows;
DROP TABLE IF EXISTS holds;
DROP TABLE IF EXISTS receipts;
DROP TABLE IF EXISTS accounts;
DROP TYPE IF EXISTS receipt_type;
-- +goose StatementEnd
