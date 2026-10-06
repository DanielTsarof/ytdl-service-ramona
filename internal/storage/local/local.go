// Package local stores objects as files under a root directory.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// metaSuffix names the sidecar holding an object's content type and metadata.
// Keys ending in it are rejected so an object can never overwrite a sidecar.
const metaSuffix = ".meta.json"

type Store struct {
	root string
}

var _ storage.Storage = (*Store)(nil)

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("local storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("local storage root: %w", err)
	}
	return &Store{root: abs}, nil
}

type sidecar struct {
	ContentType string            `json:"content_type,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
}

func (s *Store) path(key string) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	if strings.HasSuffix(key, metaSuffix) {
		return "", fmt.Errorf("%w: %q uses reserved suffix %s", storage.ErrInvalidKey, key, metaSuffix)
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

// Put writes to a temp file next to the target and renames it into place, so
// a crash or a failing reader never leaves a truncated object under key.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, info storage.ObjectInfo) (storage.ObjectInfo, error) {
	p, err := s.path(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("create dir: %w", err)
	}

	if err := writeAtomic(dir, p, func(f *os.File) error {
		_, err := io.Copy(f, ctxReader{ctx, r})
		return err
	}); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("write %s: %w", key, err)
	}

	meta, err := json.Marshal(sidecar{ContentType: info.ContentType, Meta: info.Meta})
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := writeAtomic(dir, p+metaSuffix, func(f *os.File) error {
		_, err := f.Write(meta)
		return err
	}); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("write %s metadata: %w", key, err)
	}
	return s.Stat(ctx, key)
}

func writeAtomic(dir, dst string, fill func(*os.File) error) (err error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = fill(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

func (s *Store) Open(ctx context.Context, key string, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	p, _ := s.path(key) // validated by Stat
	f, err := os.Open(p)
	if err != nil {
		return nil, storage.ObjectInfo{}, mapErr(key, err)
	}
	if rng == nil {
		return f, info, nil
	}
	if rng.Start < 0 || rng.Start >= info.Size || (rng.End >= 0 && rng.End < rng.Start) {
		f.Close()
		return nil, storage.ObjectInfo{}, fmt.Errorf("%w: %d-%d of %d", storage.ErrInvalidRange, rng.Start, rng.End, info.Size)
	}
	if _, err := f.Seek(rng.Start, io.SeekStart); err != nil {
		f.Close()
		return nil, storage.ObjectInfo{}, err
	}
	return limitedFile{io.LimitReader(f, rng.Length(info.Size)), f}, info, nil
}

func (s *Store) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	p, err := s.path(key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return storage.ObjectInfo{}, mapErr(key, err)
	}
	if fi.IsDir() {
		return storage.ObjectInfo{}, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	info := storage.ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}
	// A missing or unreadable sidecar degrades to "no metadata", not an error:
	// the object itself is intact and servable.
	if raw, err := os.ReadFile(p + metaSuffix); err == nil {
		var sc sidecar
		if json.Unmarshal(raw, &sc) == nil {
			info.ContentType, info.Meta = sc.ContentType, sc.Meta
		}
	}
	return info, nil
}

func (s *Store) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return mapErr(key, err)
	}
	if err := os.Remove(p + metaSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func mapErr(key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return err
}

type limitedFile struct {
	io.Reader
	io.Closer
}

// ctxReader stops a long copy once ctx is cancelled; os.File I/O itself
// ignores contexts.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
