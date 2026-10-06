package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/cache"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
)

const (
	ctxPrincipal = "principal"
	ctxRequestID = "request_id"
)

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestLog assigns a request ID (reusing a sane X-Request-ID) and logs one
// line per request. Query strings are logged (they carry the media URL, not
// credentials); API keys only ever travel in headers and are never logged.
func (s *Server) requestLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		id := c.GetHeader("X-Request-ID")
		if !requestIDRe.MatchString(id) {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		c.Set(ctxRequestID, id)
		c.Header("X-Request-ID", id)

		c.Next()

		attrs := []any{
			slog.String("request_id", id),
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.String("query", c.Request.URL.RawQuery),
			slog.Int("status", c.Writer.Status()),
			slog.Int("bytes", c.Writer.Size()),
			slog.Duration("elapsed", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
		}
		if p, ok := principalOf(c); ok {
			attrs = append(attrs, slog.Int64("user_id", p.User.ID), slog.String("key_prefix", p.KeyPrefix))
		}
		level := slog.LevelInfo
		if c.Writer.Status() >= 500 {
			level = slog.LevelError
		}
		s.log.Log(c.Request.Context(), level, "http request", attrs...)
	}
}

func (s *Server) recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				if r == http.ErrAbortHandler {
					panic(r) // client went away mid-response; let net/http handle it
				}
				s.log.Error("handler panic",
					slog.String("request_id", c.GetString(ctxRequestID)),
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())))
				if !c.Writer.Written() {
					abort(c, http.StatusInternalServerError, codeInternal, "internal error")
				}
			}
		}()
		c.Next()
	}
}

func principalOf(c *gin.Context) (db.Principal, bool) {
	v, ok := c.Get(ctxPrincipal)
	if !ok {
		return db.Principal{}, false
	}
	p, ok := v.(db.Principal)
	return p, ok
}

func mustPrincipal(c *gin.Context) db.Principal {
	p, _ := principalOf(c)
	return p
}

// authenticate accepts "Authorization: Bearer <key>" or "X-API-Key: <key>".
func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("X-API-Key")
		if h := c.GetHeader("Authorization"); key == "" && h != "" {
			scheme, token, _ := strings.Cut(h, " ")
			if strings.EqualFold(scheme, "Bearer") {
				key = strings.TrimSpace(token)
			}
		}
		if key == "" {
			c.Header("WWW-Authenticate", `Bearer realm="ytdl-service"`)
			abort(c, http.StatusUnauthorized, codeUnauthorized, "API key required")
			return
		}
		p, err := s.db.Authenticate(c.Request.Context(), key)
		if errors.Is(err, db.ErrInvalidAPIKey) {
			c.Header("WWW-Authenticate", `Bearer realm="ytdl-service", error="invalid_token"`)
			abort(c, http.StatusUnauthorized, codeUnauthorized, "invalid or revoked API key")
			return
		}
		if err != nil {
			s.fail(c, err)
			return
		}
		c.Set(ctxPrincipal, p)
		c.Next()
	}
}

func (s *Server) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !mustPrincipal(c).IsAdmin() {
			abort(c, http.StatusForbidden, codeForbidden, "administrator role required")
			return
		}
		c.Next()
	}
}

// rateLimit applies a GCRA limit per API key. If Redis is unreachable it
// fails open (logs and lets the request through): an outage of the limiter
// should not take the whole API down.
func (s *Server) rateLimit() gin.HandlerFunc {
	limit := redis_rate.Limit{
		Rate:   s.cfg.RateLimitPerMinute,
		Burst:  s.cfg.RateLimitBurst,
		Period: time.Minute,
	}
	return func(c *gin.Context) {
		p := mustPrincipal(c)
		res, err := s.limiter.Allow(c.Request.Context(), "ratelimit:key:"+strconv.FormatInt(p.KeyID, 10), limit)
		if err != nil {
			s.log.Warn("rate limiter unavailable, allowing request", slog.Any("err", err))
			c.Next()
			return
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit.Rate))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
		if res.Allowed == 0 {
			retry := int(res.RetryAfter.Seconds() + 0.999)
			if retry < 1 {
				retry = 1
			}
			c.Header("Retry-After", strconv.Itoa(retry))
			abort(c, http.StatusTooManyRequests, codeRateLimited, fmt.Sprintf("rate limit exceeded, retry in %ds", retry))
			return
		}
		c.Next()
	}
}

// Cache scopes. Writes bump a scope's generation, which invalidates every
// cached response in it at once.
const (
	scopeUsers   = "users"
	scopeMedia   = "media"
	scopeHistory = "history" // resolved per request to history:<user_id> or history:all
)

func historyScope(userID int64) string { return "history:" + strconv.FormatInt(userID, 10) }

// cachedResponse is what the read cache stores.
type cachedResponse struct {
	Status      int    `json:"s"`
	ContentType string `json:"ct"`
	Body        []byte `json:"b"`
}

type captureWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// cached serves JSON GET responses from Redis. The key covers the caller
// (user and role: admins and users see different data), the path and the
// sorted query, and the scope's generation. Only 200 responses are stored.
func (s *Server) cached(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		p := mustPrincipal(c)

		sc := scope
		if scope == scopeHistory {
			sc = historyScopeFor(c, p)
		}
		gen, ok := s.cache.Generation(ctx, sc)
		if !ok {
			c.Next() // Redis down: serve uncached
			return
		}
		key := cache.ResponseKey(sc, gen,
			strconv.FormatInt(p.User.ID, 10)+":"+string(p.User.Role),
			c.Request.URL.Path+"?"+canonicalQuery(c))

		if raw, ok := s.cache.Get(ctx, key); ok {
			var cr cachedResponse
			if json.Unmarshal(raw, &cr) == nil {
				c.Header("X-Cache", "HIT")
				c.Data(cr.Status, cr.ContentType, cr.Body)
				c.Abort()
				return
			}
		}

		c.Header("X-Cache", "MISS")
		w := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = w
		c.Next()
		if w.Status() != http.StatusOK || c.IsAborted() {
			return
		}
		raw, err := json.Marshal(cachedResponse{Status: w.Status(), ContentType: w.Header().Get("Content-Type"), Body: w.buf.Bytes()})
		if err == nil {
			s.cache.Set(ctx, key, raw)
		}
	}
}

// historyScopeFor picks the history scope a request reads: the caller's own
// history, a specific user's (admin), or everything (admin, no filter).
func historyScopeFor(c *gin.Context, p db.Principal) string {
	if !p.IsAdmin() {
		return historyScope(p.User.ID)
	}
	if uid, err := strconv.ParseInt(c.Query("user_id"), 10, 64); err == nil {
		return historyScope(uid)
	}
	return "history:all"
}

// canonicalQuery sorts query parameters so equivalent requests share a key.
func canonicalQuery(c *gin.Context) string {
	q := c.Request.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			b.WriteString(k + "=" + v + "&")
		}
	}
	return b.String()
}
