package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/storagetest"
)

func TestConformance(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, s, "test")
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Put(ctx, "a/b.mp3", strings.NewReader("ok"), storage.ObjectInfo{}); err != nil {
		t.Fatal(err)
	}
	failing := &failAfter{data: "partial"}
	if _, err := s.Put(ctx, "a/c.mp3", failing, storage.ObjectInfo{}); err == nil {
		t.Fatal("expected failure")
	}
	entries, err := os.ReadDir(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestReservedSuffix(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), "a/b.mp3"+metaSuffix, strings.NewReader("x"), storage.ObjectInfo{})
	if !errors.Is(err, storage.ErrInvalidKey) {
		t.Fatalf("got %v, want ErrInvalidKey", err)
	}
}

type failAfter struct {
	data string
	done bool
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.done {
		return 0, errors.New("boom")
	}
	f.done = true
	return copy(p, f.data), nil
}
