// Package storagetest is a conformance suite every storage.Storage backend
// must pass, so local and S3 stay interchangeable.
package storagetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// Run exercises s. Keys are created under prefix so runs can share a bucket.
func Run(t *testing.T, s storage.Storage, prefix string) {
	ctx := context.Background()
	key := prefix + "/media/abc/mp3.mp3"
	body := []byte("0123456789abcdefghij")
	meta := map[string]string{"title": "Привет — tést ♪", "video-id": "abc"}

	t.Run("missing", func(t *testing.T) {
		if _, err := s.Stat(ctx, prefix+"/nope"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Stat missing: got %v, want ErrNotFound", err)
		}
		if _, _, err := s.Open(ctx, prefix+"/nope", nil); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Open missing: got %v, want ErrNotFound", err)
		}
		if err := s.Delete(ctx, prefix+"/nope"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Delete missing: got %v, want ErrNotFound", err)
		}
	})

	t.Run("put and stat", func(t *testing.T) {
		// A plain io.Reader, not a *bytes.Reader: Put must not need the length.
		info, err := s.Put(ctx, key, io.MultiReader(bytes.NewReader(body)), storage.ObjectInfo{
			ContentType: "audio/mpeg", Meta: meta,
		})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if info.Size != int64(len(body)) || info.ContentType != "audio/mpeg" {
			t.Fatalf("Put info = %+v", info)
		}
		st, err := s.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if st.Size != int64(len(body)) || st.ContentType != "audio/mpeg" || st.ModTime.IsZero() {
			t.Fatalf("Stat info = %+v", st)
		}
		for k, v := range meta {
			if st.Meta[k] != v {
				t.Fatalf("Meta[%q] = %q, want %q (all: %v)", k, st.Meta[k], v, st.Meta)
			}
		}
	})

	t.Run("open whole", func(t *testing.T) {
		got, info := read(t, s, key, nil)
		if !bytes.Equal(got, body) || info.Size != int64(len(body)) {
			t.Fatalf("got %q size %d", got, info.Size)
		}
	})

	t.Run("open ranges", func(t *testing.T) {
		for _, tc := range []struct {
			rng  storage.ByteRange
			want string
		}{
			{storage.ByteRange{Start: 0, End: 3}, "0123"},
			{storage.ByteRange{Start: 10, End: -1}, "abcdefghij"},
			{storage.ByteRange{Start: 18, End: 100}, "ij"},
		} {
			got, info := read(t, s, key, &tc.rng)
			if string(got) != tc.want {
				t.Errorf("range %+v: got %q, want %q", tc.rng, got, tc.want)
			}
			if info.Size != int64(len(body)) {
				t.Errorf("range %+v: info.Size = %d, want full size %d", tc.rng, info.Size, len(body))
			}
		}
		if _, _, err := s.Open(ctx, key, &storage.ByteRange{Start: 100, End: -1}); !errors.Is(err, storage.ErrInvalidRange) {
			t.Errorf("range past EOF: got %v, want ErrInvalidRange", err)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		if _, err := s.Put(ctx, key, strings.NewReader("new"), storage.ObjectInfo{ContentType: "audio/mpeg"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if got, _ := read(t, s, key, nil); string(got) != "new" {
			t.Fatalf("after overwrite got %q", got)
		}
	})

	t.Run("failed put leaves nothing", func(t *testing.T) {
		k := prefix + "/media/abc/broken.mp3"
		r := io.MultiReader(strings.NewReader("partial"), errReader{})
		if _, err := s.Put(ctx, k, r, storage.ObjectInfo{}); err == nil {
			t.Fatal("Put with failing reader succeeded")
		}
		if _, err := s.Stat(ctx, k); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Stat after failed Put: got %v, want ErrNotFound", err)
		}
	})

	t.Run("invalid keys", func(t *testing.T) {
		for _, k := range []string{"", "/abs", "../escape", "a/../../b", "a//b", "a/./b", "a/", `a\b`} {
			if _, err := s.Put(ctx, k, strings.NewReader("x"), storage.ObjectInfo{}); !errors.Is(err, storage.ErrInvalidKey) {
				t.Errorf("Put(%q): got %v, want ErrInvalidKey", k, err)
			}
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := s.Delete(ctx, key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Stat after Delete: got %v, want ErrNotFound", err)
		}
	})
}

func read(t *testing.T, s storage.Storage, key string, rng *storage.ByteRange) ([]byte, storage.ObjectInfo) {
	t.Helper()
	rc, info, err := s.Open(context.Background(), key, rng)
	if err != nil {
		t.Fatalf("Open(%q, %+v): %v", key, rng, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b, info
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("source failed") }
