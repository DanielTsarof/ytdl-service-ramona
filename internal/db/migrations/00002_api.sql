-- +goose Up

-- Files not requested for FILE_IDLE_TTL are evicted from storage; the
-- partial index keeps that scan to rows that still have a file.
ALTER TABLE videos ADD COLUMN last_requested_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE audio  ADD COLUMN last_requested_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX videos_idle_idx ON videos (last_requested_at) WHERE storage_key IS NOT NULL;
CREATE INDEX audio_idle_idx  ON audio  (last_requested_at) WHERE storage_key IS NOT NULL;
CREATE INDEX videos_url_idx ON videos (url);
CREATE INDEX audio_url_idx  ON audio  (url, format);

-- Per-user secret that signs webhook deliveries. Kept readable (HMAC needs
-- it). The default is volatile, so every row, existing ones included, gets
-- its own 244 random bits from two gen_random_uuid() calls (PG13+ built-in,
-- CSPRNG-backed).
ALTER TABLE users ADD COLUMN webhook_secret text NOT NULL
    DEFAULT replace(gen_random_uuid()::text, '-', '') || replace(gen_random_uuid()::text, '-', '');

CREATE TYPE task_status   AS ENUM ('queued', 'running', 'succeeded', 'failed');
CREATE TYPE webhook_state AS ENUM ('none', 'pending', 'delivered', 'failed');
CREATE TYPE media_format  AS ENUM ('mp4', 'mp3', 'wav');

CREATE TABLE tasks (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          bigint        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    api_key_id       bigint        REFERENCES api_keys (id) ON DELETE SET NULL,
    -- Client-supplied Idempotency-Key, unique per user.
    idempotency_key  text,
    -- sha256 of the normalised request; detects key reuse with a different
    -- body, and doubles as the implicit idempotency fingerprint.
    request_hash     bytea         NOT NULL,
    query            text          NOT NULL DEFAULT '',
    source_url       text          NOT NULL DEFAULT '',
    format           media_format  NOT NULL,
    source_id        text,
    title            text          NOT NULL DEFAULT '',
    storage_key      text,
    status           task_status   NOT NULL DEFAULT 'queued',
    error            text,
    attempts         integer       NOT NULL DEFAULT 0,
    -- Lease of the worker running the task; an expired lease means the
    -- worker died and the task may be claimed again.
    locked_until     timestamptz,
    webhook_url      text,
    webhook_state    webhook_state NOT NULL DEFAULT 'none',
    webhook_attempts integer       NOT NULL DEFAULT 0,
    next_webhook_at  timestamptz,
    webhook_error    text,
    history_id       bigint,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    started_at       timestamptz,
    completed_at     timestamptz,
    -- The result is downloadable by task ID until this moment.
    expires_at       timestamptz,
    UNIQUE (user_id, idempotency_key)
);
CREATE INDEX tasks_claim_idx ON tasks (created_at) WHERE status IN ('queued', 'running');
CREATE INDEX tasks_webhook_idx ON tasks (next_webhook_at) WHERE webhook_state = 'pending';
CREATE INDEX tasks_fingerprint_idx ON tasks (user_id, request_hash);
CREATE INDEX tasks_storage_key_idx ON tasks (storage_key) WHERE storage_key IS NOT NULL;

CREATE TYPE request_kind   AS ENUM ('file', 'stream', 'task');
CREATE TYPE request_status AS ENUM ('pending', 'ok', 'error');

CREATE TABLE request_history (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     bigint         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    api_key_id  bigint         REFERENCES api_keys (id) ON DELETE SET NULL,
    kind        request_kind   NOT NULL,
    query       text           NOT NULL DEFAULT '',
    source_url  text           NOT NULL DEFAULT '',
    format      media_format   NOT NULL,
    source_id   text,
    title       text           NOT NULL DEFAULT '',
    status      request_status NOT NULL DEFAULT 'pending',
    http_status integer,
    -- Served from storage without downloading.
    from_cache  boolean        NOT NULL DEFAULT false,
    task_id     uuid           REFERENCES tasks (id) ON DELETE SET NULL,
    error       text,
    duration_ms bigint,
    created_at  timestamptz    NOT NULL DEFAULT now()
);
CREATE INDEX request_history_user_created_idx ON request_history (user_id, created_at DESC);
CREATE INDEX request_history_created_idx ON request_history (created_at DESC);

-- +goose Down
DROP TABLE request_history;
DROP TYPE request_status;
DROP TYPE request_kind;
DROP TABLE tasks;
DROP TYPE media_format;
DROP TYPE webhook_state;
DROP TYPE task_status;
ALTER TABLE users DROP COLUMN webhook_secret;
DROP INDEX audio_url_idx;
DROP INDEX videos_url_idx;
DROP INDEX audio_idle_idx;
DROP INDEX videos_idle_idx;
ALTER TABLE audio  DROP COLUMN last_requested_at;
ALTER TABLE videos DROP COLUMN last_requested_at;
