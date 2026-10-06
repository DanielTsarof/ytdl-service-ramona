# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Go service (module `github.com/DanielTsarof/ytdl-service-ramona`, Go 1.25.1) that downloads media with yt-dlp and serves it as MP4/MP3/WAV files or live-transcoded streams. So far only the core module exists; there is no HTTP layer or `cmd/` entrypoint yet. Much of the yt-dlp/ffmpeg logic was ported from the Ramona-go Discord bot (`../Ramona-go/modules/music`).

Runtime requirements: `yt-dlp` and `ffmpeg`/`ffprobe` on PATH.

## Commands

- Build / vet: `go build ./... && go vet ./... && go vet -tags integration ./...`
- Unit tests (no network; ffmpeg tests skip if ffmpeg is missing): `go test ./...`
- Single test: `go test ./internal/media -run '^TestStreamLive$' -v`
- Integration (real yt-dlp + YouTube): `go test -tags integration ./internal/media -run Integration -v` (`INTEGRATION_URL`, `YTDLP_COOKIES` optional)
- S3 conformance (skipped unless set): `S3_TEST_ENDPOINT=http://localhost:9000 S3_TEST_BUCKET=ytdl-test AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... go test ./internal/storage/s3`

Keep the `go` directive at 1.25.1: `go get ...@latest` can pull deps that bump it (e.g. `golang.org/x/sync` ≥ v0.21 needs Go 1.26), so pin such deps instead. Use `GOTOOLCHAIN=local` to catch this.

## Architecture

The flow is `media.Service` (in `internal/media`), which drives `ytdl`, `ffmpeg` and `storage`:

1. `Service.Resolve(ctx, query, format)` runs yt-dlp `--dump-json` and returns a `Source`. The source holds direct stream URLs (1 input, or video+audio as 2 inputs), the storage key `media/<videoID>/<fmt>.<ext>`, and `Stored` if the key already exists. Resolve also rejects live streams and anything over `MaxDuration`. Resolving is separate from acting so an HTTP layer can set headers before the first byte.
2. `Service.Fetch(ctx, src)` makes sure the file is in storage. yt-dlp downloads into a per-job temp dir under `WorkDir` (yt-dlp runs ffmpeg itself to extract or merge), and the result is `Put` into storage. Concurrent fetches of one key are deduped with `singleflight`. The download runs on a context detached from the caller's (with a 30 min cap), so a disconnecting client doesn't waste a half-done download.
3. `Service.Stream(ctx, src, w)` copies from storage if the file is stored. Otherwise it pipes a live ffmpeg transcode into `w` (fragmented MP4 / MP3 / streaming WAV; args are in `media/format.go`). It retries with re-resolved URLs **only if no byte has been written yet**, because restarting mid-stream would corrupt the container. Cancelling ctx kills ffmpeg.
4. `Service.Open(ctx, key, *ByteRange)` reads stored objects for range requests.

Storage (`internal/storage`): the `Storage` interface (Put with unknown length, ranged Open, Stat, Delete) plus an optional `URLSigner` (S3 presigned URLs). The backends are in `storage/local` (temp file + rename, metadata in a `<key>.meta.json` sidecar) and `storage/s3` (aws-sdk-go-v2 `transfermanager` multipart uploads, metadata values query-escaped because S3 metadata is ASCII headers). The factory is `storage/backends.New(ctx, cfg.Storage)`; it lives in its own package because the backends import `storage`. Every backend must pass `storage/storagetest.Run`, and new backends should add a conformance test calling it. Key rules are in `storage.ValidateKey`, shared by all backends.

`internal/ffmpeg` builds argv (reconnect/`-rw_timeout` flags for URL inputs) and logs it with URLs reduced to hosts, because googlevideo URLs carry signed tokens. Keep that redaction when touching logging.

Config is env/`.env` via cleanenv (`internal/config`): `STORAGE_BACKEND=local|s3`, `STORAGE_LOCAL_DIR`, `S3_BUCKET/REGION/ENDPOINT/PATH_STYLE/PREFIX` (credentials from the standard AWS chain), `WORK_DIR`, `MAX_DURATION_SECONDS`, `YTDLP_COOKIES`, `LOG_LEVEL`, `LOG_FORMAT`.
