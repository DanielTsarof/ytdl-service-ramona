// Package cache is the Redis-backed read cache: cached API responses
// invalidated by per-scope generation counters, and the search-name → video
// ID map that lets repeated name requests skip yt-dlp.
//
// Every operation is best effort. Redis being down degrades to "cache miss",
// never to a failed request.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// resolveTTL bounds how long a search name keeps mapping to one video; search
// results drift, so this is deliberately shorter than file retention.
const resolveTTL = 24 * time.Hour

type Cache struct {
	rdb *redis.Client
	ttl time.Duration
	log *slog.Logger
}

func New(rdb *redis.Client, ttl time.Duration, logger *slog.Logger) *Cache {
	return &Cache{rdb: rdb, ttl: ttl, log: logger}
}

func (c *Cache) TTL() time.Duration { return c.ttl }

func genKey(scope string) string { return "gen:" + scope }

// Generation returns the current generation of scope (0 if never bumped).
// Cache keys embed it, so Bump invalidates a whole scope at once without
// scanning for keys; the stale entries simply expire.
func (c *Cache) Generation(ctx context.Context, scope string) (int64, bool) {
	n, err := c.rdb.Get(ctx, genKey(scope)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, true
	}
	if err != nil {
		c.log.Warn("cache generation read failed", slog.String("scope", scope), slog.Any("err", err))
		return 0, false
	}
	return n, true
}

// Bump invalidates every cached response in the given scopes.
func (c *Cache) Bump(ctx context.Context, scopes ...string) {
	for _, s := range scopes {
		if err := c.rdb.Incr(ctx, genKey(s)).Err(); err != nil {
			c.log.Warn("cache invalidation failed", slog.String("scope", s), slog.Any("err", err))
		}
	}
}

// ResponseKey builds the key for one cached response. principal must
// distinguish callers whose responses differ (user ID and role).
func ResponseKey(scope string, gen int64, principal, request string) string {
	sum := sha256.Sum256([]byte(request))
	return "cache:" + scope + ":" + strconv.FormatInt(gen, 10) + ":" + principal + ":" + hex.EncodeToString(sum[:16])
}

func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
	b, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.log.Warn("cache read failed", slog.Any("err", err))
		}
		return nil, false
	}
	return b, true
}

func (c *Cache) Set(ctx context.Context, key string, value []byte) {
	if err := c.rdb.Set(ctx, key, value, c.ttl).Err(); err != nil {
		c.log.Warn("cache write failed", slog.Any("err", err))
	}
}

func resolveKey(kind, name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(name))))
	return "resolve:" + kind + ":" + hex.EncodeToString(sum[:])
}

// ResolvedID returns the video ID a search name resolved to recently.
func (c *Cache) ResolvedID(ctx context.Context, kind, name string) (string, bool) {
	id, err := c.rdb.Get(ctx, resolveKey(kind, name)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.log.Warn("resolve cache read failed", slog.Any("err", err))
		}
		return "", false
	}
	return id, true
}

func (c *Cache) SetResolvedID(ctx context.Context, kind, name, id string) {
	if err := c.rdb.Set(ctx, resolveKey(kind, name), id, resolveTTL).Err(); err != nil {
		c.log.Warn("resolve cache write failed", slog.Any("err", err))
	}
}
