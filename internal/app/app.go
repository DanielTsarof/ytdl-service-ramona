// Package app is the application service shared by the HTTP handlers and the
// task workers. It turns a Request into a stored file, avoiding yt-dlp
// entirely when the file is already in storage, and keeps the videos/audio
// tables in step with what storage holds.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/cache"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// Media is the subset of *media.Service the app needs; tests substitute a
// fake so no yt-dlp or network is involved.
type Media interface {
	Resolve(ctx context.Context, query string, f media.Format, q media.Quality) (media.Source, error)
	Lookup(ctx context.Context, id string, f media.Format, q media.Quality) (media.Source, bool, error)
	Fetch(ctx context.Context, src media.Source) (media.Fetched, error)
	Stream(ctx context.Context, src media.Source, w io.Writer) error
}

var _ Media = (*media.Service)(nil)

type App struct {
	Media Media
	Store storage.Storage
	DB    *db.DB
	Cache *cache.Cache
	log   *slog.Logger
}

func New(m Media, store storage.Storage, database *db.DB, c *cache.Cache, logger *slog.Logger) *App {
	return &App{Media: m, Store: store, DB: database, Cache: c, log: logger}
}

func kindName(f media.Format) string {
	if f == media.MP4 {
		return "video"
	}
	return "audio"
}

// Locate finds the source for req. When the file is already stored and the
// video ID can be found cheaply (a YouTube URL, a URL seen before, or a
// recently resolved name), no yt-dlp call is made. Otherwise it resolves,
// which also enforces the duration and live-stream limits.
func (a *App) Locate(ctx context.Context, req Request) (media.Source, error) {
	if id := a.knownID(ctx, req); id != "" {
		src, ok, err := a.Media.Lookup(ctx, id, req.Format, req.Quality)
		if err != nil {
			return media.Source{}, err
		}
		if ok {
			return src, nil
		}
	}
	src, err := a.Media.Resolve(ctx, req.Query(), req.Format, req.Quality)
	if err != nil {
		return media.Source{}, err
	}
	if req.Name != "" {
		a.Cache.SetResolvedID(ctx, kindName(req.Format), req.Name, src.Info.ID)
	}
	return src, nil
}

func (a *App) knownID(ctx context.Context, req Request) string {
	if req.URL == "" {
		id, _ := a.Cache.ResolvedID(ctx, kindName(req.Format), req.Name)
		return id
	}
	if id := YouTubeID(req.URL); id != "" {
		return id
	}
	var (
		id  string
		err error
	)
	if req.Format == media.MP4 {
		var v dbgen.Video
		v, err = a.DB.GetVideoByURL(ctx, req.URL)
		id = v.SourceID
	} else {
		var au dbgen.Audio
		au, err = a.DB.GetAudioByURL(ctx, dbgen.GetAudioByURLParams{Url: req.URL, Format: dbgen.AudioFormat(req.Format)})
		id = au.SourceID
	}
	if err != nil && !db.IsNotFound(err) {
		a.log.Warn("catalog lookup by URL failed", slog.Any("err", err))
	}
	return id
}

// Result is a file ready to serve.
type Result struct {
	Source media.Source
	Object storage.ObjectInfo
	// FromCache is true when the file was already stored (no download).
	FromCache bool
}

// Get returns the stored file for req, downloading it first if needed. A
// file already in storage is never fetched again.
func (a *App) Get(ctx context.Context, req Request) (Result, error) {
	src, err := a.Locate(ctx, req)
	if err != nil {
		return Result{}, err
	}
	fetched, err := a.Media.Fetch(ctx, src)
	if err != nil {
		return Result{}, err
	}
	src.Stored = &fetched.Object
	a.Record(ctx, src, fetched.Downloaded)
	return Result{Source: src, Object: fetched.Object, FromCache: !fetched.Downloaded}, nil
}

// Record keeps the catalog in step with storage: a fresh upload upserts the
// row; serving an existing file bumps last_requested_at, which is what keeps
// it from idle eviction. Failures are logged, not returned: the file itself
// was served fine.
func (a *App) Record(ctx context.Context, src media.Source, downloaded bool) {
	ctx = context.WithoutCancel(ctx)
	l := a.log.With(slog.String("source_id", src.Info.ID), slog.String("format", string(src.Format)),
		slog.String("quality", string(src.Quality)))

	if !downloaded {
		n, err := a.touch(ctx, src)
		if err != nil {
			l.Warn("recording request failed", slog.Any("err", err))
			return
		}
		if n > 0 {
			return
		}
		// Stored but uncatalogued (e.g. evicted row, or stored before the
		// catalog existed): fall through and create the row so cleanup can
		// track the file.
	}
	if err := a.upsert(ctx, src); err != nil {
		l.Warn("recording upload failed", slog.Any("err", err))
		return
	}
	a.Cache.Bump(ctx, "media")
}

func (a *App) touch(ctx context.Context, src media.Source) (int64, error) {
	if src.Format == media.MP4 {
		return a.DB.TouchVideoRequested(ctx, dbgen.TouchVideoRequestedParams{
			SourceID: src.Info.ID, Quality: VideoQuality(src.Quality),
		})
	}
	return a.DB.TouchAudioRequested(ctx, dbgen.TouchAudioRequestedParams{
		SourceID: src.Info.ID, Format: dbgen.AudioFormat(src.Format),
	})
}

func (a *App) upsert(ctx context.Context, src media.Source) error {
	var dur *int32
	if src.Info.Duration > 0 {
		d := int32(src.Info.Duration)
		dur = &d
	}
	key := src.Key
	link := src.Info.WebpageURL
	if link == "" {
		link = "https://www.youtube.com/watch?v=" + src.Info.ID
	}
	if src.Format == media.MP4 {
		_, err := a.DB.UpsertVideo(ctx, dbgen.UpsertVideoParams{
			SourceID: src.Info.ID, Quality: VideoQuality(src.Quality), Url: link, Title: src.Info.Title, DurationSeconds: dur, StorageKey: &key,
		})
		return err
	}
	_, err := a.DB.UpsertAudio(ctx, dbgen.UpsertAudioParams{
		SourceID: src.Info.ID, Format: dbgen.AudioFormat(src.Format), Url: link,
		Title: src.Info.Title, DurationSeconds: dur, StorageKey: &key,
	})
	return err
}

// VideoQuality converts q to its database enum; the zero value is best.
func VideoQuality(q media.Quality) dbgen.VideoQuality {
	if q == "" {
		return dbgen.VideoQualityBest
	}
	return dbgen.VideoQuality(q)
}

// Open reads a stored object (optionally a byte range).
func (a *App) Open(ctx context.Context, key string, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return a.Store.Open(ctx, key, rng)
}

// DeleteStored removes a stored file; a file that is already gone is fine.
func (a *App) DeleteStored(ctx context.Context, key string) error {
	if err := a.Store.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return nil
}
