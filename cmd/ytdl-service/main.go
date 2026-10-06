// Command ytdl-service runs the HTTP API and its background jobs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/api"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/cache"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/config"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/jobs"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/logging"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/backends"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("err", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := logging.Setup(cfg.LogLevel, cfg.LogFormat)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log.Info("starting ytdl-service", cfg.LogAttrs()...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	database, err := db.Open(startCtx, cfg.DatabaseURL, log.With(slog.String("component", "db")))
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Migrate(startCtx); err != nil {
		return err
	}

	redisOpts, err := redis.ParseURL(cfg.HTTP.RedisURL)
	if err != nil {
		return fmt.Errorf("REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()
	if err := rdb.Ping(startCtx).Err(); err != nil {
		// Not fatal: the cache and rate limiter degrade gracefully, and
		// /readyz reports Redis as unavailable.
		log.Warn("redis unreachable at startup", slog.Any("err", err))
	}

	store, err := backends.New(startCtx, cfg.Storage)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	mediaSvc, err := media.New(
		ytdl.New(cfg.YtdlpCookies, log.With(slog.String("component", "ytdl"))),
		store,
		media.Options{WorkDir: cfg.WorkDir, MaxDuration: cfg.MaxDuration()},
		log.With(slog.String("component", "media")),
	)
	if err != nil {
		return err
	}
	c := cache.New(rdb, cfg.Limits.CacheTTL, log.With(slog.String("component", "cache")))
	application := app.New(mediaSvc, store, database, c, log.With(slog.String("component", "app")))

	if err := bootstrapAdmin(startCtx, database, cfg.Admin, log); err != nil {
		return err
	}

	runner := jobs.New(application, jobs.Config{
		Workers:             cfg.Jobs.TaskWorkers,
		TaskLease:           cfg.Jobs.TaskLease,
		TaskResultTTL:       cfg.Jobs.TaskResultTTL,
		FileIdleTTL:         cfg.Jobs.FileIdleTTL,
		CleanupInterval:     cfg.Jobs.CleanupInterval,
		WebhookTimeout:      cfg.Jobs.WebhookTimeout,
		WebhookMaxAttempts:  cfg.Jobs.WebhookMaxAttempts,
		WebhookAllowPrivate: cfg.Jobs.WebhookAllowPrivate,
		WorkDir:             cfg.WorkDir,
	}, log.With(slog.String("component", "jobs")))
	jobsCtx, stopJobs := context.WithCancel(context.Background())
	runner.Start(jobsCtx)

	srv := api.New(application, rdb, runner, api.Config{
		RateLimitPerMinute:  cfg.Limits.RateLimitPerMinute,
		RateLimitBurst:      cfg.Limits.RateLimitBurst,
		WebhookAllowPrivate: cfg.Jobs.WebhookAllowPrivate,
	}, log.With(slog.String("component", "http")))
	httpServer := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: downloads and live streams legitimately run for
		// minutes. Client disconnects cancel the request context instead.
		IdleTimeout: 2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", cfg.HTTP.Addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-errCh:
		stopJobs()
		runner.Wait()
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown incomplete", slog.Any("err", err))
	}
	// Running tasks are abandoned, not failed: their lease lapses and a
	// worker (after restart, or on another instance) picks them up again.
	stopJobs()
	runner.Wait()
	log.Info("stopped")
	return nil
}

// bootstrapAdmin creates the first administrator when BOOTSTRAP_ADMIN_EMAIL
// is set and no user has that email yet. A generated key is logged exactly
// once, on the run that creates the admin.
func bootstrapAdmin(ctx context.Context, database *db.DB, cfg config.BootstrapAdmin, log *slog.Logger) error {
	if cfg.Email == "" {
		return nil
	}
	key, created, err := database.BootstrapAdmin(ctx, cfg.Username, cfg.Email, cfg.APIKey)
	if err != nil {
		return err
	}
	switch {
	case !created:
		log.Debug("bootstrap admin already exists", slog.String("email", cfg.Email))
	case key != "":
		log.Warn("bootstrap admin created; store this API key now, it will not be shown again",
			slog.String("email", cfg.Email), slog.String("api_key", key))
	default:
		log.Info("bootstrap admin created with the configured API key", slog.String("email", cfg.Email))
	}
	return nil
}
