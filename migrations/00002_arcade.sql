-- +goose Up
-- +goose StatementBegin

-- Game sessions.
--
-- /points/earn takes a client-supplied amount from a browser, bounded only by a
-- per-call and per-day ceiling. Within those bounds a forged request looks
-- exactly like real play. A session gives the server something to check the
-- claim against: when the run started, which game it was, and whether the score
-- is achievable in the elapsed time.
--
-- Sessions are additive. The thirteen existing game builds post a bare
-- {amount, gameId, gameName} with no session id and cannot be redeployed on this
-- service's schedule, so earning must keep working without one.
CREATE TABLE game_sessions (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         text        NOT NULL,
    game_id         text        NOT NULL,
    started_at      timestamptz NOT NULL DEFAULT now(),
    settled_at      timestamptz,
    -- What the client claimed. Kept even when refused: a run of rejections from
    -- one player is the signal worth acting on, and discarding the evidence
    -- makes that invisible.
    reported_score  bigint,
    -- Points actually awarded, which may be 0 when the daily budget is spent.
    accepted_amount bigint,
    status          text        NOT NULL DEFAULT 'open'
                                CHECK (status IN ('open', 'settled', 'rejected')),

    -- A settled session must record when and what; an open one must not.
    CONSTRAINT session_settlement_is_consistent
        CHECK ((status = 'open') = (settled_at IS NULL))
);

-- Sweeping and debugging both look for a player's open sessions.
CREATE INDEX game_sessions_open_idx ON game_sessions (user_id, started_at DESC)
    WHERE status = 'open';

-- Leaderboard entries. One row per settled run; the boards reduce these to a
-- personal best per player at query time.
CREATE TABLE scores (
    id         bigserial   PRIMARY KEY,
    user_id    text        NOT NULL,
    game_id    text        NOT NULL,
    score      bigint      NOT NULL CHECK (score >= 0),
    -- The session that produced it, so a suspicious score can be traced back to
    -- its timing. Nullable because scores may later arrive from paths without a
    -- session.
    session_id uuid        REFERENCES game_sessions (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Backs the per-game board: highest first, oldest winning a tie so the first
-- player to reach a score keeps the position.
CREATE INDEX scores_game_rank_idx ON scores (game_id, score DESC, created_at ASC);

-- Backs the windowed and global boards, which filter by recency across games.
CREATE INDEX scores_recent_idx ON scores (created_at DESC);

-- One score per settled session. A replayed settle is already refused in code;
-- this makes a duplicate impossible rather than merely unlikely.
CREATE UNIQUE INDEX scores_one_per_session_idx ON scores (session_id)
    WHERE session_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS scores;
DROP TABLE IF EXISTS game_sessions;
-- +goose StatementEnd
