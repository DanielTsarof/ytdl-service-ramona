# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Go service (module `github.com/DanielTsarof/ytdl-service-ramona`, Go 1.25.1) that downloads media with yt-dlp and serves it over an HTTP API (Gin) as MP4/MP3/WAV files, live-transcoded streams, or async tasks delivered by webhook. Much of the yt-dlp/ffmpeg logic was ported from the Ramona-go Discord bot (`../Ramona-go/modules/music`).

Runtime requirements: `ffmpeg` on PATH, PostgreSQL, and Redis. yt-dlp is installed by the service itself at startup (see Docker below). Modern yt-dlp also needs `deno` for YouTube. All settings are in `.env.example`.

## Commands

- Run: `go run ./cmd/ytdl-service` (reads env or `.env`; migrates the DB on startup)
- Full stack (app + Postgres + Redis + RustFS S3): `cp .env.example .env && docker compose up -d --build`
- Build / vet: `go build ./... && go vet ./... && go vet -tags integration ./...`
- Unit tests (no services, no network; ffmpeg tests skip without ffmpeg): `go test ./...`
- Single test: `go test ./internal/api -run '^TestTaskLifecycleAndIdempotency$' -v`
- Tests that need Postgres and Redis (skipped unless set). Each test creates and drops its own database, so the role needs CREATEDB. The Redis database in `TEST_REDIS_URL` is **flushed** by every API test:
  ```
  docker run -d --rm --name ytdl-pg-test -e POSTGRES_PASSWORD=test -p 15432:5432 postgres:17-alpine
  docker run -d --rm --name ytdl-redis-test -p 16379:6379 redis:7-alpine
  TEST_DATABASE_URL='postgres://postgres:test@localhost:15432/postgres?sslmode=disable' \
  TEST_REDIS_URL='redis://localhost:16379/15' go test -race ./...
  ```
- Integration (real yt-dlp + YouTube): `go test -tags integration ./internal/media -run Integration -v` (`INTEGRATION_URL` and `YTDLP_COOKIES` are optional)
- S3 conformance (skipped unless set; the bucket is created if missing):
  ```
  docker run -d --rm --name ytdl-s3-test -p 19100:9000 -e RUSTFS_ACCESS_KEY=testkey -e RUSTFS_SECRET_KEY=testsecret123 rustfs/rustfs:1.0.1
  S3_TEST_ENDPOINT=http://localhost:19100 AWS_ACCESS_KEY_ID=testkey AWS_SECRET_ACCESS_KEY=testsecret123 go test ./internal/storage/s3
  ```
- Regenerate sqlc code after editing `internal/db/queries/*.sql` or migrations: `go generate ./internal/db` (needs cgo; sqlc runs via `go run` at a pinned version and is not in go.mod)

Keep the `go` directive at 1.25.1: `go get ...@latest` can pull deps that bump it, so pin such deps instead, and use `GOTOOLCHAIN=local` to catch this. Current pins held back for this reason:
- `golang.org/x/sync` v0.20.0
- `pressly/goose/v3` v3.27.0 (v3.27.1+ needs 1.25.7)
- `redis/go-redis/v9` v9.22.0 (v9.23+ needs 1.26)
- sqlc v1.30.0 (v1.31+ needs 1.26)

## Docker

- `Dockerfile` is multi-stage: a static `CGO_ENABLED=0` build (cross-compiled for multi-arch) goes onto `alpine:3.23` with `ffmpeg` and `deno`. It runs as uid 10001; the image is about 290 MB.
- yt-dlp follows the Ramona-go pattern and is **not** in the image:
  - `main.go` calls `ytdlp.Install(AllowVersionMismatch)`, which downloads `yt-dlp_musllinux` into `~/.cache/go-ytdlp` (the `ytdlp-cache` volume) on first start.
  - `selfUpdateYtdlp` then runs `UpdateTo("stable@latest")` in the background. It is turned off with `YTDLP_SELF_UPDATE=false`.
  - The file keeps go-ytdlp's pinned version in its name after an update; the binary inside is current.
- In `compose.yaml`, the app's `environment:` sets the service wiring (DB, Redis, S3 endpoint, `S3_CREATE_BUCKET=true`) and overrides `.env`.
- RustFS (`rustfs/rustfs:1.0.1`) is the bundled S3. MinIO is no longer published on Docker Hub. Its console is on `127.0.0.1:9001`.
- Redis runs without persistence, with `volatile-lru`, so cache generation counters are never evicted.
- `./data` is mounted read-write at `/data` for `YTDLP_COOKIES`. It must be writable by uid 10001.

## Architecture

The layers, top to bottom: `cmd/ytdl-service` (wiring and shutdown) → `internal/api` (Gin handlers and middleware) and `internal/jobs` (background work) → `internal/app` (retrieval logic shared by both) → `internal/media`, `internal/storage`, `internal/db` and `internal/cache`.

### Retrieval (`internal/app`, `internal/media`)
- **`app.ParseRequest`**: `url` takes priority over `name` (a search query), and `format` is one of mp4, mp3 or wav.
- **`app.Locate`** first looks for a cheap video ID: a YouTube URL is parsed with `app.YouTubeID`, any other URL is looked up in the catalog by `url`, and a name is looked up in the Redis resolve cache. If that ID's file is in storage (`media.Service.Lookup`), yt-dlp never runs. Otherwise it calls `media.Service.Resolve`, which enforces the duration and live-stream limits.
- **`app.Get`** = Locate, then `Fetch`, then `Record`:
  - `Fetch` downloads only if the file isn't stored, deduped with singleflight, and reports `Downloaded`.
  - `Record` upserts the `videos`/`audio` row after a download, or bumps `last_requested_at` on a cache hit. That timestamp is what keeps a file from idle eviction, so **every path that serves a stored file must call `Record`** (the handlers do).
- **`media.Service`**:
  - `Resolve` runs yt-dlp `--dump-json` and returns a `Source`: stream URLs, the storage key `media/<videoID>/<fmt>.<ext>`, and `Stored`.
  - `Fetch` downloads via yt-dlp into `WorkDir`, then `Put`s the result into storage. The download is detached from the caller's context, with a 30-minute cap.
  - `Stream` runs a live ffmpeg transcode; its args are in `media/format.go`. It retries only before the first byte is written.
  - `internal/ffmpeg` logs its argv with URLs reduced to hosts, because the URLs carry signed tokens.
- **Storage** (`internal/storage`):
  - The interface is `Put` (unknown length), ranged `Open`, `Stat` and `Delete`, with the backends in `local` and `s3` and the factory in `backends.New`.
  - Every backend must pass `storagetest.Run`.
  - Key rules are in `storage.ValidateKey`.

### HTTP API (`internal/api`)
- **Routes** are all in `server.go`.
  - Auth: `Authorization: Bearer ytdl_…` or `X-API-Key`.
  - Everything under the `admin` group requires the admin role. That covers **all** data-modifying CRUD and anything the caller doesn't own.
  - Users get `/files`, `/stream`, `/tasks`, `/me` and their own `/history`.
- **Middleware** order on `/v1` is: authenticate, then the rate limit (GCRA via `redis_rate`, per API key, fails open if Redis is down), then the handler.
- **Read cache**: `cached(scope)` stores JSON GET responses in Redis.
  - The key includes the caller's user ID and role, the path, the sorted query, and the scope's generation.
  - Writes must invalidate: call `cache.Bump(scope)` with `users`, `media`, `history:<uid>` or `history:all`. Forgetting this serves stale data until `CACHE_TTL` runs out.
  - Never put secrets behind `cached`; webhook secrets and newly issued keys are `no-store`.
- **Serving files**: `serve.go` adapts storage to an `io.ReadSeeker` for `http.ServeContent`, which provides Range, If-None-Match, If-Range and HEAD. Live streams go through `flushWriter`; once a byte is written, errors can no longer be returned as JSON.
- **Errors** are always `{"error":{"code","message"}}` via `abort` and `fail`. `classify` maps sentinel errors to statuses, so unknown errors become a 500 without leaking detail. yt-dlp failures are wrapped by `upstream()` and become 502.
- **Response types**: handlers never serialise `dbgen` structs, because `User` contains `webhook_secret`. Use the types in `dto.go`.
- **Request history** (`startHistory`/`finishHistory`) records `/files`, `/stream` and `POST /tasks`. For tasks, the worker finishes the row.
- **Idempotency** for `POST /tasks`:
  - With an `Idempotency-Key`, a repeat returns the same task; the same key with a different body returns 422.
  - Without a key, `request_hash` dedupes an identical request while its task is running or its result is still downloadable.
  - GET retrieval is idempotent by nature.

### Background jobs (`internal/jobs`, one `Runner`)
- **Task workers**:
  - Workers claim tasks with `ClaimTask` (`FOR UPDATE SKIP LOCKED`) and keep a lease alive with a heartbeat.
  - A task whose lease expires is reclaimed, up to 3 attempts; then `FailAbandonedTasks` marks it failed.
  - On shutdown, running tasks are left to their lease, not failed.
  - A finished task's file is downloadable until `expires_at` (`TASK_RESULT_TTL`).
- **Webhooks**:
  - The file is spooled to a temp file to compute its sha256, then POSTed with `X-Ytdl-Signature: t=…,v1=hex(HMAC-SHA256(user.webhook_secret, "<t>.<task_id>.<sha256>"))`. `Sign` and `VerifySignature` are the reference implementations.
  - Retries back off; failed tasks send a JSON `task.failed` event.
  - The SSRF guard lives in `NewWebhookClient`: a dial-time IP check, no redirects, no proxy. `WEBHOOK_ALLOW_PRIVATE=true` turns it off, for dev and tests only.
- **Cleanup**: `CleanupOnce` runs under a pg advisory lock, so only one instance does it. It deletes files whose `last_requested_at` is older than `FILE_IDLE_TTL` and clears their `storage_key`, keeping the catalog row. Files pinned by an unexpired task are skipped.

### Database (`internal/db`: pgx/v5, sqlc, goose)
- **Migrations** are `migrations/0000N_name.sql` with Up and Down sections. They are embedded and applied by `DB.Migrate` at startup. Every migration needs a working Down, because `TestMigrateUpDownUp` checks it.
- **Queries** are in `queries/*.sql`, and `dbgen/` is generated from them; never edit `dbgen/` by hand.
  - Don't mix `$N` and `sqlc.arg()` in one query.
  - Optional filters use `sqlc.narg` with `IS NULL OR`.
  - Durations are passed as seconds and added to `now()` in SQL, so the database clock is the only clock.
- **sqlc type mappings**:
  - Nullable columns map to pointers.
  - `uuid` maps to `google/uuid`.
  - `timestamptz` maps to `time.Time`.
- **Tables**:
  - `videos` (mp4) and `audio` (mp3/wav), keyed by yt-dlp video ID, with `storage_key` NULL when not stored.
  - `users`: role `user` or `admin`, plus a per-user `webhook_secret`.
  - `api_keys`: stores only sha256(key).
  - `tasks`
  - `request_history`
- **Helpers**: `db.IsNotFound(err)` covers `pgx.ErrNoRows`. `db.Principal` is the authenticated caller. `db.BootstrapAdmin` creates the first admin from `BOOTSTRAP_ADMIN_*`.
- `internal/db/dbtest.New(t)` gives other packages a throwaway migrated database.
