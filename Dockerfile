# syntax=docker/dockerfile:1
ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.23

# ── build ────────────────────────────────────────────────
# Runs on the build host's platform and cross-compiles: no cgo is involved
# (pgx is pure Go), so multi-arch images need no emulation.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ytdl-service ./cmd/ytdl-service

# ── runtime ──────────────────────────────────────────────
FROM alpine:${ALPINE_VERSION}

# ffmpeg: live transcoding and yt-dlp post-processing (extract/merge).
# deno:   JS runtime modern yt-dlp needs to solve YouTube's challenges;
#         without it formats go missing.
# yt-dlp itself is not baked in: the service installs it into the go-ytdlp
# cache on first start (musl build) and self-updates it, so it keeps up with
# YouTube without image rebuilds. Mount a volume on the cache directory.
RUN apk add --no-cache ffmpeg deno ca-certificates tzdata \
 && adduser -D -u 10001 -h /home/app app \
 && mkdir -p /home/app/.cache/go-ytdlp /work /data \
 && chown -R app:app /home/app /work /data

COPY --from=build /out/ytdl-service /usr/local/bin/ytdl-service

USER app
WORKDIR /home/app
ENV HTTP_ADDR=:8080 \
    WORK_DIR=/work \
    STORAGE_LOCAL_DIR=/data/media

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
    CMD wget -qO /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/ytdl-service"]
