# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Go service (module `github.com/DanielTsarof/ytdl-service-ramona`, Go 1.25.1) that downloads media with yt-dlp and serves it as MP4/MP3/WAV files or live-transcoded streams. So far only the core module exists; there is no HTTP layer or `cmd/` entrypoint yet. Much of the yt-dlp/ffmpeg logic was ported from the Ramona-go Discord bot (`../Ramona-go/modules/music`).

Runtime requirements: `yt-dlp` and `ffmpeg`/`ffprobe` on PATH, and PostgreSQL (`DATABASE_URL`).

## Commands

- Build / vet: `go build ./... && go vet ./... && go vet -tags integration ./...`
- Unit tests (no network; ffmpeg tests skip if ffmpeg is missing): `go test ./...`
- Single test: `go test ./internal/media -run '^TestStreamLive$' -v`
- Integration (real yt-dlp + YouTube): `go test -tags integration ./internal/media -run Integration -v` (`INTEGRATION_URL`, `YTDLP_COOKIES` optional)
- S3 conformance (skipped unless set): `S3_TEST_ENDPOINT=http://localhost:9000 S3_TEST_BUCKET=ytdl-test AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... go test ./internal/storage/s3`

- DB tests (skipped unless set; each test creates and drops its own database, so the role needs CREATEDB):
  `docker run -d --rm --name ytdl-pg-test -e POSTGRES_PASSWORD=test -p 15432:5432 postgres:17-alpine`
  `TEST_DATABASE_URL='postgres://postgres:test@localhost:15432/postgres?sslmode=disable' go test ./internal/db -v`
- Regenerate sqlc code after editing `internal/db/queries/*.sql` or migrations: `go generate ./internal/db` (needs cgo; sqlc runs via `go run` at a pinned version and is not in go.mod)

Keep the `go` directive at 1.25.1: `go get ...@latest` can pull deps that bump it, so pin such deps instead. Use `GOTOOLCHAIN=local` to catch this. Current pins held back for this reason: `golang.org/x/sync` v0.20.0, `pressly/goose/v3` v3.27.0 (v3.27.1+ needs 1.25.7), sqlc v1.30.0 (v1.31+ needs 1.26).

## Architecture

The flow is `media.Service` (in `internal/media`), which drives `ytdl`, `ffmpeg` and `storage`:

1. `Service.Resolve(ctx, query, format)` runs yt-dlp `--dump-json` and returns a `Source`. The source holds direct stream URLs (1 input, or video+audio as 2 inputs), the storage key `media/<videoID>/<fmt>.<ext>`, and `Stored` if the key already exists. Resolve also rejects live streams and anything over `MaxDuration`. Resolving is separate from acting so an HTTP layer can set headers before the first byte.
2. `Service.Fetch(ctx, src)` makes sure the file is in storage. yt-dlp downloads into a per-job temp dir under `WorkDir` (yt-dlp runs ffmpeg itself to extract or merge), and the result is `Put` into storage. Concurrent fetches of one key are deduped with `singleflight`. The download runs on a context detached from the caller's (with a 30 min cap), so a disconnecting client doesn't waste a half-done download.
3. `Service.Stream(ctx, src, w)` copies from storage if the file is stored. Otherwise it pipes a live ffmpeg transcode into `w` (fragmented MP4 / MP3 / streaming WAV; args are in `media/format.go`). It retries with re-resolved URLs **only if no byte has been written yet**, because restarting mid-stream would corrupt the container. Cancelling ctx kills ffmpeg.
4. `Service.Open(ctx, key, *ByteRange)` reads stored objects for range requests.

Storage (`internal/storage`): the `Storage` interface (Put with unknown length, ranged Open, Stat, Delete) plus an optional `URLSigner` (S3 presigned URLs). The backends are in `storage/local` (temp file + rename, metadata in a `<key>.meta.json` sidecar) and `storage/s3` (aws-sdk-go-v2 `transfermanager` multipart uploads, metadata values query-escaped because S3 metadata is ASCII headers). The factory is `storage/backends.New(ctx, cfg.Storage)`; it lives in its own package because the backends import `storage`. Every backend must pass `storage/storagetest.Run`, and new backends should add a conformance test calling it. Key rules are in `storage.ValidateKey`, shared by all backends.

`internal/ffmpeg` builds argv (reconnect/`-rw_timeout` flags for URL inputs) and logs it with URLs reduced to hosts, because googlevideo URLs carry signed tokens. Keep that redaction when touching logging.

Database (`internal/db`) uses pgx/v5, sqlc and goose:
- **Migrations** are `migrations/0000N_name.sql` with `-- +goose Up` and `-- +goose Down` sections. They are embedded into the binary and applied by `DB.Migrate`; `DB.MigrateDownTo` is for tests and recovery. The same files also work with the goose CLI. Every migration needs a working Down, because `TestMigrateUpDownUp` checks it.
- **Queries** live in `queries/*.sql`, and sqlc generates `dbgen/` from them. Never edit `dbgen/` by hand. `DB` embeds `*dbgen.Queries`, so generated methods are called on it directly; `InTx` wraps them in a transaction.
- **sqlc settings**: nullable columns map to pointers (`*string`, `*time.Time`) and `timestamptz` maps to `time.Time`; both are set in `sqlc.yaml`.
- **Schema**:
  - `videos` holds MP4 rows, unique on `source_id`.
  - `audio` holds one row per `(source_id, format)`, with format `mp3` or `wav`.
  - Media rows are keyed by the yt-dlp video ID, the same ID `media.Key` uses, not by URL. `storage_key` is NULL when the file isn't stored, and an upsert refreshes `last_uploaded_at`.
  - `users` has a unique username, a case-insensitive unique email (matched with `lower(email)`), and a `user_role` enum (`user` or `admin`).
  - `api_keys` belongs to a user and stores only `sha256(key)`. The plaintext key looks like `ytdl_…` and is returned once, by `DB.CreateAPIKey(email, name)`. `DB.Authenticate` returns `ErrInvalidAPIKey` for unknown and revoked keys alike.
- Generated queries return `pgx.ErrNoRows` for missing rows. Use `db.IsNotFound(err)`, which accepts that and `db.ErrNotFound`.

Config is env/`.env` via cleanenv (`internal/config`): `DATABASE_URL` (required; the startup log shows only user@host:port/db), `STORAGE_BACKEND=local|s3`, `STORAGE_LOCAL_DIR`, `S3_BUCKET/REGION/ENDPOINT/PATH_STYLE/PREFIX` (credentials from the standard AWS chain), `WORK_DIR`, `MAX_DURATION_SECONDS`, `YTDLP_COOKIES`, `LOG_LEVEL`, `LOG_FORMAT`.
