// Package storage defines where finished media files are saved. Backends live
// in subpackages (local, s3); backends.New picks one from config.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

var (
	ErrNotFound   = errors.New("storage: object not found")
	ErrInvalidKey = errors.New("storage: invalid key")
	// ErrInvalidRange is returned by Open when the range starts past the end
	// of the object (HTTP 416 territory).
	ErrInvalidRange = errors.New("storage: range not satisfiable")
)

// ObjectInfo describes a stored object. On Put, ContentType and Meta are taken
// from the caller and Size/ModTime are filled in by the backend.
type ObjectInfo struct {
	Key         string
	Size        int64
	ContentType string
	ModTime     time.Time
	Meta        map[string]string
}

// ByteRange selects part of an object. End is inclusive; End < 0 means "to EOF".
type ByteRange struct {
	Start, End int64
}

// Length returns the number of bytes the range covers in an object of size
// total, after clamping End to the last byte.
func (r ByteRange) Length(total int64) int64 {
	end := r.End
	if end < 0 || end >= total {
		end = total - 1
	}
	return end - r.Start + 1
}

type Storage interface {
	// Put stores r under key. r may be of unknown length; readers never see a
	// partially written object.
	Put(ctx context.Context, key string, r io.Reader, info ObjectInfo) (ObjectInfo, error)
	// Open returns the object's content (or the part rng selects; nil = whole
	// object). The returned ObjectInfo describes the whole object, not the range.
	Open(ctx context.Context, key string, rng *ByteRange) (io.ReadCloser, ObjectInfo, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

// URLSigner is an optional capability: backends that can hand out a
// time-limited direct download URL (S3) implement it, so callers can redirect
// instead of proxying bytes.
type URLSigner interface {
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ValidateKey enforces the key format shared by all backends: a relative,
// slash-separated, already-clean path with no "." or ".." elements. Keeping
// one rule means a key valid on S3 is always valid on disk and vice versa.
func ValidateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") ||
		strings.ContainsRune(key, 0) || path.Clean(key) != key {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	for _, el := range strings.Split(key, "/") {
		if el == "." || el == ".." {
			return fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
	}
	return nil
}
