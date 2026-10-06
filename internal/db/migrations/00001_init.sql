-- +goose Up
CREATE TYPE user_role AS ENUM ('user', 'admin');
CREATE TYPE audio_format AS ENUM ('mp3', 'wav');

CREATE TABLE users (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      text        NOT NULL UNIQUE,
    email         text        NOT NULL,
    role          user_role   NOT NULL DEFAULT 'user',
    registered_at timestamptz NOT NULL DEFAULT now()
);
-- Case-insensitive uniqueness without the citext extension; queries match
-- with lower(email) so they use this index.
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

-- API keys authenticate users. Only sha256(key) is stored: keys are 256-bit
-- random tokens, so a fast hash is enough and the plaintext is shown once.
CREATE TABLE api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key_hash     bytea       NOT NULL UNIQUE,
    -- First characters of the plaintext key, to tell keys apart in listings.
    prefix       text        NOT NULL,
    name         text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_keys_user_id_idx ON api_keys (user_id);

-- Media rows are keyed by the yt-dlp video ID, not the link: many URLs and
-- search queries resolve to the same video (the storage cache key uses the
-- ID too). storage_key is NULL while the file is not in storage.
CREATE TABLE videos (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id        text        NOT NULL UNIQUE,
    url              text        NOT NULL,
    title            text        NOT NULL DEFAULT '',
    duration_seconds integer,
    storage_key      text,
    last_uploaded_at timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audio (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id        text         NOT NULL,
    format           audio_format NOT NULL,
    url              text         NOT NULL,
    title            text         NOT NULL DEFAULT '',
    duration_seconds integer,
    storage_key      text,
    last_uploaded_at timestamptz  NOT NULL DEFAULT now(),
    created_at       timestamptz  NOT NULL DEFAULT now(),
    UNIQUE (source_id, format)
);

-- +goose Down
DROP TABLE audio;
DROP TABLE videos;
DROP TABLE api_keys;
DROP TABLE users;
DROP TYPE audio_format;
DROP TYPE user_role;
