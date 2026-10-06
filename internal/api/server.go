// Package api is the HTTP layer (Gin). Handlers are thin: retrieval goes
// through app.App, background work through jobs.Runner, persistence through
// db.DB.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/cache"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
)

type Config struct {
	RateLimitPerMinute  int
	RateLimitBurst      int
	WebhookAllowPrivate bool
}

// Waker is told when a task is enqueued (jobs.Runner.Wake).
type Waker interface{ Wake() }

type Server struct {
	app     *app.App
	db      *db.DB
	cache   *cache.Cache
	rdb     *redis.Client
	limiter *redis_rate.Limiter
	waker   Waker
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
}

func New(a *app.App, rdb *redis.Client, waker Waker, cfg Config, logger *slog.Logger) *Server {
	return &Server{
		app:     a,
		db:      a.DB,
		cache:   a.Cache,
		rdb:     rdb,
		limiter: redis_rate.NewLimiter(rdb),
		waker:   waker,
		cfg:     cfg,
		log:     logger,
		now:     time.Now,
	}
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.Use(s.requestLog(), s.recovery())
	r.NoRoute(func(c *gin.Context) { abort(c, http.StatusNotFound, codeNotFound, "no such endpoint") })
	r.NoMethod(func(c *gin.Context) { abort(c, http.StatusMethodNotAllowed, codeBadRequest, "method not allowed") })

	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)

	v1 := r.Group("/v1", s.authenticate(), s.rateLimit())

	// Retrieval: every authenticated user.
	v1.GET("/files", s.getFile)
	v1.HEAD("/files", s.getFile)
	v1.GET("/stream", s.stream)
	v1.POST("/tasks", s.createTask)
	v1.GET("/tasks/:id", s.getTask)
	v1.GET("/tasks/:id/file", s.getTaskFile)
	v1.HEAD("/tasks/:id/file", s.getTaskFile)

	// Own account and history.
	v1.GET("/me", s.cached(scopeUsers), s.me)
	v1.GET("/me/webhook-secret", s.myWebhookSecret)
	v1.POST("/me/webhook-secret/rotate", s.rotateMyWebhookSecret)
	v1.GET("/history", s.cached(scopeHistory), s.history)

	// Administration: all data-modifying CRUD and everything not owned by
	// the caller.
	admin := v1.Group("", s.requireAdmin())
	admin.GET("/users", s.cached(scopeUsers), s.listUsers)
	admin.POST("/users", s.createUser)
	admin.GET("/users/:id", s.cached(scopeUsers), s.getUser)
	admin.PATCH("/users/:id", s.updateUser)
	admin.DELETE("/users/:id", s.deleteUser)
	admin.POST("/users/:id/webhook-secret/rotate", s.rotateUserWebhookSecret)

	admin.GET("/users/:id/api-keys", s.cached(scopeUsers), s.listAPIKeys)
	admin.POST("/users/:id/api-keys", s.createAPIKey)
	admin.POST("/users/:id/api-keys/revoke-all", s.revokeAllAPIKeys)
	admin.POST("/users/:id/api-keys/:key_id/revoke", s.revokeAPIKey)
	admin.POST("/users/:id/api-keys/:key_id/refresh", s.refreshAPIKey)

	admin.GET("/videos", s.cached(scopeMedia), s.listVideos)
	admin.GET("/videos/:source_id", s.cached(scopeMedia), s.getVideo)
	admin.DELETE("/videos/:source_id", s.deleteVideo)
	admin.GET("/audio", s.cached(scopeMedia), s.listAudio)
	admin.GET("/audio/:source_id/:format", s.cached(scopeMedia), s.getAudio)
	admin.DELETE("/audio/:source_id/:format", s.deleteAudio)

	return r
}

func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz reports whether the dependencies needed to serve requests answer.
func (s *Server) readyz(c *gin.Context) {
	ctx := c.Request.Context()
	checks := gin.H{"database": "ok", "redis": "ok"}
	status := http.StatusOK
	if err := s.db.Pool.Ping(ctx); err != nil {
		checks["database"], status = "unavailable", http.StatusServiceUnavailable
	}
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		checks["redis"], status = "unavailable", http.StatusServiceUnavailable
	}
	c.JSON(status, checks)
}
