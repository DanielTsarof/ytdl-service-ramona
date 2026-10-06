package jobs

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

// cleanupLockID is the pg advisory lock that makes exactly one instance run
// a cleanup pass at a time. Arbitrary, but must stay stable.
const cleanupLockID = 0x79746463 // "ytdc"

const cleanupBatch = 100

func (r *Runner) runCleanup(ctx context.Context) {
	for {
		if _, err := r.CleanupOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("cleanup pass failed", slog.Any("err", err))
		}
		if !sleep(ctx, r.cfg.CleanupInterval) {
			return
		}
	}
}

// CleanupOnce deletes stored files nobody requested within FileIdleTTL and
// clears their storage_key (catalog rows are kept). Files referenced by a
// task whose result is still downloadable are skipped. It returns the
// number of files removed; 0 with nil error when another instance holds the
// lock.
func (r *Runner) CleanupOnce(ctx context.Context) (int, error) {
	conn, err := r.db.Pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", cleanupLockID).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		r.log.Debug("cleanup skipped: another instance is running it")
		return 0, nil
	}
	defer func() {
		// Unlock on the same session that took the lock.
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", cleanupLockID); err != nil {
			r.log.Warn("releasing cleanup lock failed", slog.Any("err", err))
		}
	}()

	start := time.Now()
	cutoff := start.Add(-r.cfg.FileIdleTTL)
	q := dbgen.New(conn)

	videos, err := r.cleanVideos(ctx, q, cutoff)
	if err != nil {
		return videos, err
	}
	audio, err := r.cleanAudio(ctx, q, cutoff)
	total := videos + audio
	if total > 0 {
		r.app.Cache.Bump(context.WithoutCancel(ctx), "media")
	}
	r.log.Info("cleanup pass finished",
		slog.Int("videos_removed", videos),
		slog.Int("audio_removed", audio),
		slog.Time("cutoff", cutoff),
		slog.Duration("elapsed", time.Since(start)))
	return total, err
}

func (r *Runner) cleanVideos(ctx context.Context, q *dbgen.Queries, cutoff time.Time) (int, error) {
	removed := 0
	for {
		rows, err := q.ListIdleVideos(ctx, dbgen.ListIdleVideosParams{Cutoff: cutoff, BatchSize: cleanupBatch})
		if err != nil || len(rows) == 0 {
			return removed, err
		}
		for _, v := range rows {
			if err := r.app.DeleteStored(ctx, *v.StorageKey); err != nil {
				return removed, err
			}
			if _, err := q.ClearVideoStorageKey(ctx, dbgen.ClearVideoStorageKeyParams{SourceID: v.SourceID, Quality: v.Quality}); err != nil {
				return removed, err
			}
			removed++
			r.log.Debug("evicted idle video", slog.String("source_id", v.SourceID), slog.String("quality", string(v.Quality)), slog.Time("last_requested_at", v.LastRequestedAt))
		}
		if len(rows) < cleanupBatch {
			return removed, nil
		}
	}
}

func (r *Runner) cleanAudio(ctx context.Context, q *dbgen.Queries, cutoff time.Time) (int, error) {
	removed := 0
	for {
		rows, err := q.ListIdleAudio(ctx, dbgen.ListIdleAudioParams{Cutoff: cutoff, BatchSize: cleanupBatch})
		if err != nil || len(rows) == 0 {
			return removed, err
		}
		for _, a := range rows {
			if err := r.app.DeleteStored(ctx, *a.StorageKey); err != nil {
				return removed, err
			}
			if _, err := q.ClearAudioStorageKey(ctx, dbgen.ClearAudioStorageKeyParams{SourceID: a.SourceID, Format: a.Format}); err != nil {
				return removed, err
			}
			removed++
			r.log.Debug("evicted idle audio",
				slog.String("source_id", a.SourceID), slog.String("format", string(a.Format)),
				slog.Time("last_requested_at", a.LastRequestedAt))
		}
		if len(rows) < cleanupBatch {
			return removed, nil
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
