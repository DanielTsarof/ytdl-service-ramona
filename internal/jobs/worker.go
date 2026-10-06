// Package jobs runs the background work: async task workers, webhook
// delivery and the idle-file cleanup. All three coordinate through Postgres
// (SKIP LOCKED claims, leases, an advisory lock), so any number of service
// instances can run them side by side.
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// maxTaskAttempts bounds how often a task is retried after its worker died
// mid-run. Ordinary failures (bad URL, yt-dlp error) are not retried.
const maxTaskAttempts = 3

type Config struct {
	Workers             int
	TaskLease           time.Duration
	TaskResultTTL       time.Duration
	FileIdleTTL         time.Duration
	CleanupInterval     time.Duration
	WebhookTimeout      time.Duration
	WebhookMaxAttempts  int
	WebhookAllowPrivate bool
	// WorkDir holds webhook spool files.
	WorkDir string
}

type Runner struct {
	app    *app.App
	db     *db.DB
	store  storage.Storage
	cfg    Config
	client *http.Client
	log    *slog.Logger
	wake   chan struct{}
	wg     sync.WaitGroup
	// workDir is cfg.WorkDir; kept separately for the webhook helpers.
	workDir string
}

func New(a *app.App, cfg Config, logger *slog.Logger) *Runner {
	return &Runner{
		app:     a,
		db:      a.DB,
		store:   a.Store,
		cfg:     cfg,
		client:  NewWebhookClient(cfg.WebhookTimeout, cfg.WebhookAllowPrivate),
		log:     logger,
		wake:    make(chan struct{}, 1),
		workDir: cfg.WorkDir,
	}
}

// Wake nudges an idle worker to poll now instead of at its next tick; called
// when this instance enqueues a task.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Start launches workers, webhook delivery and cleanup. They stop when ctx
// is cancelled; Wait blocks until they have.
func (r *Runner) Start(ctx context.Context) {
	for i := 0; i < r.cfg.Workers; i++ {
		r.wg.Add(1)
		go func(n int) {
			defer r.wg.Done()
			r.runWorker(ctx, r.log.With(slog.Int("worker", n)))
		}(i)
	}
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		r.runWebhooks(ctx)
	}()
	go func() {
		defer r.wg.Done()
		r.runCleanup(ctx)
	}()
	r.log.Info("background jobs started",
		slog.Int("workers", r.cfg.Workers),
		slog.Duration("file_idle_ttl", r.cfg.FileIdleTTL),
		slog.Duration("cleanup_interval", r.cfg.CleanupInterval))
}

func (r *Runner) Wait() { r.wg.Wait() }

func (r *Runner) runWorker(ctx context.Context, l *slog.Logger) {
	lease := r.cfg.TaskLease.Seconds()
	lastSweep := time.Time{}
	for {
		if time.Since(lastSweep) > r.cfg.TaskLease {
			lastSweep = time.Now()
			if n, err := r.db.FailAbandonedTasks(ctx, maxTaskAttempts); err != nil && ctx.Err() == nil {
				l.Warn("failing abandoned tasks failed", slog.Any("err", err))
			} else if n > 0 {
				l.Warn("failed abandoned tasks", slog.Int64("count", n))
			}
		}

		task, err := r.db.ClaimTask(ctx, dbgen.ClaimTaskParams{LeaseSeconds: lease, MaxAttempts: maxTaskAttempts})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !db.IsNotFound(err) {
				l.Error("claiming task failed", slog.Any("err", err))
			}
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			case <-time.After(time.Second):
			}
			continue
		}
		r.runTask(ctx, l.With(slog.String("task_id", task.ID.String())), task)
	}
}

func (r *Runner) runTask(ctx context.Context, l *slog.Logger, task dbgen.Task) {
	start := time.Now()
	l.Info("task started", slog.Int("attempt", int(task.Attempts)), slog.String("format", string(task.Format)))

	// Heartbeat: keep the lease alive while yt-dlp works, so no other worker
	// takes the task over.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(r.cfg.TaskLease / 3)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if _, err := r.db.ExtendTaskLease(runCtx, dbgen.ExtendTaskLeaseParams{
					ID: task.ID, LeaseSeconds: r.cfg.TaskLease.Seconds(),
				}); err != nil && runCtx.Err() == nil {
					l.Warn("extending task lease failed", slog.Any("err", err))
				}
			}
		}
	}()

	req := app.Request{URL: task.SourceUrl, Name: task.Query, Format: media.Format(task.Format), Quality: media.Quality(task.Quality)}
	res, err := r.app.Get(runCtx, req)

	// Shutting down: leave the task "running"; once the lease lapses another
	// worker (here after restart, or another instance) picks it up again.
	if ctx.Err() != nil {
		l.Info("task interrupted by shutdown; will be retried after lease expiry")
		return
	}

	bg := context.WithoutCancel(ctx)
	if err != nil {
		l.Warn("task failed", slog.Duration("elapsed", time.Since(start)), slog.Any("err", err))
		if _, ferr := r.db.FailTask(bg, dbgen.FailTaskParams{ID: task.ID, Error: ptr(publicError(err))}); ferr != nil {
			l.Error("recording task failure failed", slog.Any("err", ferr))
		}
		r.finishHistory(bg, l, task, dbgen.RequestStatusError, nil, false, err)
		return
	}

	key := res.Source.Key
	if _, err := r.db.CompleteTask(bg, dbgen.CompleteTaskParams{
		ID:               task.ID,
		SourceID:         &res.Source.Info.ID,
		Title:            res.Source.Info.Title,
		StorageKey:       &key,
		ResultTtlSeconds: r.cfg.TaskResultTTL.Seconds(),
	}); err != nil {
		l.Error("recording task completion failed", slog.Any("err", err))
		return
	}
	l.Info("task succeeded",
		slog.String("source_id", res.Source.Info.ID),
		slog.Bool("from_cache", res.FromCache),
		slog.Duration("elapsed", time.Since(start)))
	r.finishHistory(bg, l, task, dbgen.RequestStatusOk, &res, res.FromCache, nil)
}

func (r *Runner) finishHistory(ctx context.Context, l *slog.Logger, task dbgen.Task, status dbgen.RequestStatus, res *app.Result, fromCache bool, taskErr error) {
	if task.HistoryID == nil {
		return
	}
	p := dbgen.FinishHistoryParams{ID: *task.HistoryID, Status: status, FromCache: fromCache}
	if res != nil {
		p.SourceID = &res.Source.Info.ID
		p.Title = res.Source.Info.Title
	}
	if taskErr != nil {
		p.Error = ptr(publicError(taskErr))
	}
	if err := r.db.FinishHistory(ctx, p); err != nil {
		l.Warn("updating request history failed", slog.Any("err", err))
		return
	}
	r.app.Cache.Bump(ctx, "history:"+itoa(task.UserID), "history:all")
}

// publicError is the failure message stored for the user: specific for
// known causes, generic otherwise (yt-dlp/ffmpeg output can be long and
// leak internal paths).
func publicError(err error) string {
	switch {
	case errors.Is(err, media.ErrTooLong), errors.Is(err, media.ErrLive), errors.Is(err, app.ErrInvalidRequest):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return "could not retrieve the media"
}

// PublicError exposes publicError to the HTTP layer.
func PublicError(err error) string { return publicError(err) }
