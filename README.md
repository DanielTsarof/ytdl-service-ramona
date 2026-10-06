# ytdl-service-ramona

An HTTP service that downloads media with [yt-dlp](https://github.com/yt-dlp/yt-dlp) and serves it as ready-to-use **MP4, MP3 or WAV** files. It can return a file directly, transcode it live as a stream, or run an asynchronous task that sends the result to a signed webhook.

Features:
- **Find media by link or by name.** `url` takes priority, and `name` searches and takes the first result.
- **Stored once, served many times.** Files are kept in S3 (any S3-compatible store) or on local disk and are never downloaded again while stored. Files nobody has requested for `FILE_IDLE_TTL` (7 days by default) are deleted.
- **Live streaming** through ffmpeg, without waiting for a full download.
- **Async tasks.** Poll the task by ID, or receive the file by signed webhook. Results stay downloadable for `TASK_RESULT_TTL` (1 hour by default). Task creation is idempotent.
- **API keys tied to users**, with two roles, `user` and `admin`. Admins manage users and their keys (issue, revoke, refresh).
- Per-key **rate limiting** and a **Redis read cache**.
- **Request history** with pagination, filters and date sorting.
- An **OpenAPI 3** spec with Swagger UI at `/docs`.

## Quick start (Docker)

```sh
cp .env.example .env
# edit .env: set BOOTSTRAP_ADMIN_EMAIL, change POSTGRES_PASSWORD and S3_SECRET_KEY
docker compose up -d --build
```

This starts the app, PostgreSQL 17, Redis 7 and [RustFS](https://github.com/rustfs/rustfs) (a bundled S3-compatible store).

| What | Where |
|---|---|
| API and docs | http://localhost:8080 (`HTTP_PORT`), docs at http://localhost:8080/docs |
| RustFS web console | http://127.0.0.1:9001 (`S3_CONSOLE_PORT`) |
| PostgreSQL | `127.0.0.1:5432` (`POSTGRES_PORT`) |

On the first start the service downloads yt-dlp into the `ytdlp-cache` volume, which can take a minute. After that it updates yt-dlp in the background on every start.

The first admin is created from `BOOTSTRAP_ADMIN_*`. If you didn't set `BOOTSTRAP_ADMIN_API_KEY`, a key is generated and **logged once**:

```sh
docker compose logs app | grep api_key
export KEY=ytdl_...   # the value of api_key
```

Optional: to pass cookies to yt-dlp, put a Netscape `cookies.txt` in `./data/` and set `YTDLP_COOKIES=/data/cookies.txt`. The directory must be writable by uid 10001.

## Usage

All `/v1` endpoints need an API key: `Authorization: Bearer $KEY` or `X-API-Key: $KEY`.

### Get a file

```sh
# by link
curl -OJ -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/v1/files?url=https://www.youtube.com/watch?v=jNQXAC9IVRw&format=mp3"

# by name (search)
curl -OJ -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/v1/files?name=me+at+the+zoo&format=mp4"

# video capped at 720p
curl -OJ -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/v1/files?name=me+at+the+zoo&format=mp4&quality=720"
```

The first request downloads and stores the file. Later requests are served from storage. `HEAD`, `Range` and `If-None-Match` work as usual.

**Video quality:** `quality` is one of `360`, `480`, `720`, `1080` or `best` (the default). It caps the smaller side of the frame, so `720` gives 1280x720 for a landscape video and 720x1280 for a vertical one. You get the largest resolution at or below the cap, or the smallest above it if nothing smaller exists. Each quality is stored as its own file. `quality` is ignored for `mp3` and `wav`, and works the same on `/v1/stream` and `POST /v1/tasks`.

Stored MP4s are always H.264 video with AAC audio, so they play in any player.

### Stream

```sh
curl -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/v1/stream?name=me+at+the+zoo&format=mp3" | ffplay -
```

### Async task

```sh
curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: my-request-1' \
  -d '{"name":"me at the zoo","format":"mp3"}' \
  http://localhost:8080/v1/tasks
# → 202 {"id":"…","status":"queued","status_url":"/v1/tasks/…", …}

curl -s -H "Authorization: Bearer $KEY" http://localhost:8080/v1/tasks/<id>
# when "status" is "succeeded", download file_url (until expires_at)
curl -OJ -H "Authorization: Bearer $KEY" http://localhost:8080/v1/tasks/<id>/file
```

The body takes `url` or `name` (`url` wins if both are given), plus `format`, an optional `quality` and an optional `webhook_url`.

Repeating a request with the same `Idempotency-Key` returns the same task. Reusing a key with a different body returns `422`. Without a key, an identical request returns the existing task while it is running or while its result can still be downloaded.

### Webhooks

When `webhook_url` is set, the service POSTs the file bytes there when the task succeeds, or a JSON `task.failed` event when it fails. Every request carries these headers:

| Header | Value |
|---|---|
| `X-Ytdl-Event` | `task.succeeded` or `task.failed` |
| `X-Ytdl-Task-Id` | the task ID |
| `X-Ytdl-Content-Sha256` | hex SHA-256 of the body |
| `X-Ytdl-Signature` | `t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<task_id>.<sha256>")>` |

To verify a request:
1. Hash the body as it arrives.
2. Compare the hash with `X-Ytdl-Content-Sha256`.
3. Check the signature with your secret, and reject old timestamps.

`jobs.VerifySignature` in [internal/jobs/webhook.go](internal/jobs/webhook.go) is the reference implementation.

Get your secret with `GET /v1/me/webhook-secret`, and rotate it with `POST /v1/me/webhook-secret/rotate`. Failed deliveries are retried with backoff (`WEBHOOK_MAX_ATTEMPTS`). Webhook URLs must be public: private and loopback addresses are refused unless `WEBHOOK_ALLOW_PRIVATE=true`.

### History

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/v1/history?kind=task&status=ok&from=2026-10-01&sort=-created_at&page=1&page_size=20"
```

| Filter | Values |
|---|---|
| `kind` | `file`, `stream` or `task` |
| `status` | `pending`, `ok` or `error` |
| `format` | `mp4`, `mp3` or `wav` |
| `from`, `to` | RFC 3339 or `YYYY-MM-DD` |
| `q` | substring of the query, URL or title |

Users see only their own history. Admins see everyone's, or one user's with `user_id`.

## API overview

The full contract is in [internal/api/openapi.yaml](internal/api/openapi.yaml). The running service also serves it at `/openapi.json` and `/openapi.yaml`, and as interactive docs at `/docs`. These three paths and `/healthz` and `/readyz` are public.

| Who | Endpoints |
|---|---|
| Any user | `GET/HEAD /v1/files`, `GET /v1/stream`, `POST /v1/tasks`, `GET /v1/tasks/{id}`, `GET/HEAD /v1/tasks/{id}/file`, `GET /v1/me`, `GET /v1/me/webhook-secret`, `POST /v1/me/webhook-secret/rotate`, `GET /v1/history` |
| Admin only | `/v1/users` (CRUD, webhook secret rotation), `/v1/users/{id}/api-keys` (issue, list, revoke, revoke-all, refresh), `/v1/videos` and `/v1/audio` (catalog list, get, delete; videos have one entry per quality, and deleting without `?quality=` removes all of them) |

Errors are always `{"error": {"code": "…", "message": "…"}}`.

Responses carry `X-RateLimit-Limit` and `X-RateLimit-Remaining`. A `429` comes with `Retry-After`.

Cached reads have an `X-Cache: HIT|MISS` header.

## Configuration

All settings are environment variables, read from the environment or from `.env`. The full annotated list is in [.env.example](.env.example).

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | (required) | PostgreSQL DSN; migrations run on startup |
| `REDIS_URL` | `redis://localhost:6379/0` | Rate limiting and the read cache. If Redis is down, requests are allowed and served uncached |
| `HTTP_ADDR` | `:8080` | Listen address |
| `STORAGE_BACKEND` | `local` | `local` (`STORAGE_LOCAL_DIR`, default `./data`) or `s3` |
| `S3_BUCKET`, `S3_REGION`, `S3_ENDPOINT`, `S3_PATH_STYLE`, `S3_PREFIX`, `S3_CREATE_BUCKET` | | S3 settings; credentials come from the standard AWS chain |
| `FILE_IDLE_TTL` / `CLEANUP_INTERVAL` | `168h` / `1h` | Delete files not requested for this long, checked this often |
| `TASK_RESULT_TTL` | `1h` | How long a finished task's file stays downloadable |
| `TASK_WORKERS`, `TASK_LEASE` | `4`, `2m` | Async task workers |
| `WEBHOOK_TIMEOUT`, `WEBHOOK_MAX_ATTEMPTS`, `WEBHOOK_ALLOW_PRIVATE` | `60s`, `6`, `false` | Webhook delivery |
| `RATE_LIMIT_PER_MINUTE`, `RATE_LIMIT_BURST` | `60`, `20` | Per API key |
| `CACHE_TTL` | `30s` | Maximum staleness of cached reads |
| `MAX_DURATION_SECONDS` | `10800` | Reject longer sources (`0` = no limit) |
| `YTDLP_COOKIES`, `YTDLP_SELF_UPDATE` | empty, `true` | yt-dlp cookies file and background self-update |
| `WORK_DIR` | `<tmp>/ytdl-service` | Scratch space for downloads |
| `BOOTSTRAP_ADMIN_EMAIL`, `_USERNAME`, `_API_KEY` | empty, `admin`, empty | The first admin |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `text` | Logging |

Durations use Go syntax (`90s`, `30m`, `168h`); there is no `d` unit.

Docker compose notes:
- `compose.yaml` sets the service wiring itself (`DATABASE_URL`, `REDIS_URL`, the S3 endpoint and credentials, `WORK_DIR`), overriding `.env`. Use the `POSTGRES_*`, `S3_ACCESS_KEY`/`S3_SECRET_KEY` and `*_PORT` variables instead.
- Leave an empty value as a bare `KEY=`. Compose reads `KEY=   # comment` as the value `# comment`.
- `POSTGRES_*` are applied only when the `pgdata` volume is first created. To change them later, recreate the volume, which deletes the database: `docker compose down && docker volume rm ytdl-service_pgdata`.

## Running without Docker

You need Go 1.25.1, `ffmpeg` and `deno` (yt-dlp uses deno for YouTube) on `PATH`, plus PostgreSQL and Redis.

```sh
cp .env.example .env   # set DATABASE_URL, REDIS_URL and the storage settings
go run ./cmd/ytdl-service
```

yt-dlp is downloaded automatically into `~/.cache/go-ytdlp` unless it is already there or on `PATH`.

## Development

```sh
go build ./... && go vet ./... && go vet -tags integration ./...
go test ./...                     # unit tests: no services, no network
```

Some tests need PostgreSQL and Redis; they are skipped unless the variables below are set. Each test creates and drops its own database. The Redis database is **flushed**.

```sh
docker run -d --rm --name ytdl-pg-test -e POSTGRES_PASSWORD=test -p 15432:5432 postgres:17-alpine
docker run -d --rm --name ytdl-redis-test -p 16379:6379 redis:7-alpine
TEST_DATABASE_URL='postgres://postgres:test@localhost:15432/postgres?sslmode=disable' \
TEST_REDIS_URL='redis://localhost:16379/15' go test -race ./...
```

Other test suites:

```sh
# real yt-dlp against YouTube
go test -tags integration ./internal/media -run Integration -v

# S3 backend conformance
docker run -d --rm --name ytdl-s3-test -p 19100:9000 -e RUSTFS_ACCESS_KEY=testkey -e RUSTFS_SECRET_KEY=testsecret123 rustfs/rustfs:1.0.1
S3_TEST_ENDPOINT=http://localhost:19100 AWS_ACCESS_KEY_ID=testkey AWS_SECRET_ACCESS_KEY=testsecret123 go test ./internal/storage/s3
```

- **Database code:** after editing `internal/db/queries/*.sql` or a migration, run `go generate ./internal/db` (sqlc, needs cgo).
- **API changes:** any change to a route, parameter, status code or response field must update `internal/api/openapi.yaml`. The tests check that routes and the spec match and validate every response against the spec.
- **Go version:** keep the `go` directive at 1.25.1. Some dependencies are pinned for this; see [CLAUDE.md](CLAUDE.md).

## Project layout

```
cmd/ytdl-service    wiring, startup (yt-dlp install, migrations), graceful shutdown
internal/api        Gin handlers, middleware, openapi.yaml, /docs
internal/jobs       task workers, webhook delivery, idle-file cleanup
internal/app        retrieval logic shared by the API and the jobs
internal/media      resolve, download and stream media (yt-dlp + ffmpeg)
internal/ytdl       yt-dlp client and binary installer
internal/ffmpeg     ffmpeg process helpers
internal/storage    storage interface with local and s3 backends
internal/db         pgx, sqlc queries, goose migrations
internal/cache      Redis read cache
internal/config     environment configuration
```

## Credits

The yt-dlp and ffmpeg handling was ported from the music module of the Ramona-go Discord bot.
