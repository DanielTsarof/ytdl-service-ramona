// Package media is the core of the service: it turns a URL or search query
// into a stored file (Fetch) or a live transcoded stream (Stream).
//
// Callers resolve first, then act on the Source:
//
//	src, err := svc.Resolve(ctx, query, media.MP3) // title, cache status, key
//	obj, err := svc.Fetch(ctx, src)                // file in storage, then svc.Open(...)
//	err = svc.Stream(ctx, src, w)                  // or: bytes straight to w
//
// The split lets an HTTP layer set headers (title, content type) before the
// first byte is written.
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/ffmpeg"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

var (
	ErrTooLong = errors.New("media: source exceeds maximum duration")
	ErrLive    = errors.New("media: live streams are not supported")
)

// fetchTimeout bounds one download+upload. It runs detached from the
// requesting context (see Fetch), so it needs its own limit.
const fetchTimeout = 30 * time.Minute

type Options struct {
	// WorkDir holds per-download scratch directories.
	WorkDir string
	// MaxDuration rejects longer sources before any download (0 = no limit).
	MaxDuration time.Duration
}

type Service struct {
	yt    *ytdl.Client
	store storage.Storage
	opts  Options
	log   *slog.Logger
	sf    singleflight.Group
}

func New(yt *ytdl.Client, store storage.Storage, opts Options, logger *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("work dir: %w", err)
	}
	return &Service{yt: yt, store: store, opts: opts, log: logger}, nil
}

// Source is a resolved request: what will be served and where it is cached.
type Source struct {
	Info   ytdl.Info
	Format Format
	Key    string
	// Stored is set when the file is already in storage.
	Stored *storage.ObjectInfo
}

// Resolve looks up query and checks whether the requested format is already
// stored. Cache keys use the video ID, so a URL and a search query that land
// on the same video share one stored file.
func (s *Service) Resolve(ctx context.Context, query string, f Format) (Source, error) {
	info, err := s.yt.Resolve(ctx, query, f.Kind())
	if err != nil {
		return Source{}, err
	}
	if info.IsLive {
		return Source{}, ErrLive
	}
	if max := s.opts.MaxDuration; max > 0 && time.Duration(info.Duration)*time.Second > max {
		return Source{}, fmt.Errorf("%w: %ds > %s", ErrTooLong, info.Duration, max)
	}

	src := Source{Info: info, Format: f, Key: Key(info.ID, f)}
	switch obj, err := s.store.Stat(ctx, src.Key); {
	case err == nil:
		src.Stored = &obj
	case !errors.Is(err, storage.ErrNotFound):
		return Source{}, fmt.Errorf("storage stat: %w", err)
	}
	return src, nil
}

// Key is the storage key for video id in format f.
func Key(id string, f Format) string {
	return "media/" + safeID(id) + "/" + string(f) + "." + f.Ext()
}

// safeID keeps IDs from any extractor usable as a single key element.
func safeID(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, id)
	if strings.Trim(clean, ".") == "" {
		return "_" + clean
	}
	return clean
}

// Fetch makes sure src is in storage, downloading it if needed, and returns
// the stored object. Concurrent Fetches of the same key share one download.
// The download runs detached from ctx: a caller that gives up only stops
// waiting, and the finished file still lands in the cache for the next one.
func (s *Service) Fetch(ctx context.Context, src Source) (storage.ObjectInfo, error) {
	if src.Stored != nil {
		return *src.Stored, nil
	}
	ch := s.sf.DoChan(src.Key, func() (any, error) {
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		return s.download(workCtx, src)
	})
	select {
	case <-ctx.Done():
		return storage.ObjectInfo{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return storage.ObjectInfo{}, res.Err
		}
		return res.Val.(storage.ObjectInfo), nil
	}
}

func (s *Service) download(ctx context.Context, src Source) (storage.ObjectInfo, error) {
	l := s.log.With(slog.String("key", src.Key), slog.String("title", src.Info.Title))

	// A flight that finished between Resolve and now already stored it.
	if obj, err := s.store.Stat(ctx, src.Key); err == nil {
		return obj, nil
	}

	start := time.Now()
	dir, err := os.MkdirTemp(s.opts.WorkDir, safeID(src.Info.ID)+"-*")
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("work dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			l.Warn("removing work dir failed", slog.String("dir", dir), slog.Any("err", err))
		}
	}()

	url := src.Info.WebpageURL
	if url == "" {
		url = "https://www.youtube.com/watch?v=" + src.Info.ID
	}
	path, err := s.yt.Download(ctx, url, src.Format.target(), dir)
	if err != nil {
		l.Error("download failed", slog.Duration("elapsed", time.Since(start)), slog.Any("err", err))
		return storage.ObjectInfo{}, err
	}
	downloaded := time.Since(start)

	f, err := os.Open(path)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	defer f.Close()
	obj, err := s.store.Put(ctx, src.Key, f, storage.ObjectInfo{
		ContentType: src.Format.MIME(),
		Meta: map[string]string{
			"video-id":   src.Info.ID,
			"title":      src.Info.Title,
			"source-url": src.Info.WebpageURL,
			"duration":   strconv.Itoa(src.Info.Duration),
		},
	})
	if err != nil {
		l.Error("storing download failed", slog.Any("err", err))
		return storage.ObjectInfo{}, fmt.Errorf("store: %w", err)
	}
	l.Info("media stored",
		slog.Int64("size", obj.Size),
		slog.Duration("download", downloaded),
		slog.Duration("total", time.Since(start)))
	return obj, nil
}

// Open reads a stored object, optionally a byte range of it (for seeking
// players and resumable downloads).
func (s *Service) Open(ctx context.Context, key string, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return s.store.Open(ctx, key, rng)
}

// Stream writes src to w as it is produced. A stored file is copied from
// storage; otherwise ffmpeg transcodes the source stream on the fly, so
// playback starts without waiting for a full download. Cancelling ctx (the
// client went away) kills ffmpeg.
func (s *Service) Stream(ctx context.Context, src Source, w io.Writer) error {
	if src.Stored != nil {
		rc, _, err := s.store.Open(ctx, src.Key, nil)
		if err == nil {
			defer rc.Close()
			_, err = io.Copy(w, rc)
			return err
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		// Deleted since Resolve: fall through to a live transcode.
	}

	l := s.log.With(slog.String("key", src.Key), slog.String("title", src.Info.Title))
	info := src.Info

	// Stream URLs go stale (IP-locked, expiring), so a failure is retried
	// once with a freshly resolved URL, but only if nothing has reached w
	// yet. After that, restarting would splice a second container into the
	// middle of the first.
	const attempts = 2
	for i := 1; ; i++ {
		start := time.Now()
		cw := &countingWriter{w: w}
		err := s.streamOnce(ctx, l, info, src.Format, cw)
		if err == nil {
			l.Info("stream completed", slog.Int64("bytes", cw.n), slog.Duration("elapsed", time.Since(start)))
			return nil
		}
		if ctx.Err() != nil {
			l.Debug("stream cancelled", slog.Int64("bytes", cw.n), slog.Duration("elapsed", time.Since(start)))
			return ctx.Err()
		}
		if cw.n > 0 || i >= attempts {
			l.Error("stream failed", slog.Int64("bytes", cw.n), slog.Int("attempt", i), slog.Any("err", err))
			return err
		}
		if info.WebpageURL == "" {
			l.Warn("stream failed before first byte, retrying", slog.Any("err", err))
			continue
		}
		l.Warn("stream failed before first byte, re-resolving", slog.Any("err", err))
		fresh, rerr := s.yt.Resolve(ctx, info.WebpageURL, src.Format.Kind())
		if rerr != nil {
			l.Warn("re-resolve failed, reusing old URLs", slog.Any("err", rerr))
		} else {
			info = fresh
		}
	}
}

func (s *Service) streamOnce(parent context.Context, l *slog.Logger, info ytdl.Info, f Format, w io.Writer) error {
	// Child context so a write error (client gone) stops ffmpeg without the
	// caller having to.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	proc, err := ffmpeg.Start(ctx, l, ffmpeg.Spec{
		Inputs:  info.Inputs,
		OutArgs: f.liveArgs(info),
		Output:  ffmpeg.Pipe,
	})
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(w, proc.Stdout)
	if copyErr != nil {
		cancel()
	}
	waitErr := proc.Wait()
	if copyErr != nil {
		return fmt.Errorf("stream write: %w", copyErr)
	}
	return waitErr
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
