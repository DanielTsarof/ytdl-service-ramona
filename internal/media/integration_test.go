//go:build integration

package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/local"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

// Real yt-dlp + network. Run with:
//
//	go test -tags integration ./internal/media/ -run Integration -v
//
// INTEGRATION_URL overrides the source; keep it short (download time).
func integrationURL() string {
	if u := os.Getenv("INTEGRATION_URL"); u != "" {
		return u
	}
	return "https://www.youtube.com/watch?v=jNQXAC9IVRw" // "Me at the zoo", 19 s
}

func TestIntegrationFetchAllFormats(t *testing.T) {
	requireFFmpeg(t)
	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(ytdl.New(os.Getenv("YTDLP_COOKIES"), discard), store,
		Options{WorkDir: t.TempDir(), MaxDuration: 10 * time.Minute}, discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	want := map[Format]string{MP4: "video", MP3: "audio", WAV: "audio"}
	for _, f := range Formats {
		t.Run(string(f), func(t *testing.T) {
			src, err := svc.Resolve(ctx, integrationURL(), f)
			if err != nil {
				t.Fatal(err)
			}
			if src.Stored != nil {
				t.Fatal("fresh store reports a cached object")
			}
			obj, err := svc.Fetch(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			if obj.Size == 0 || obj.ContentType != f.MIME() || obj.Meta["video-id"] != src.Info.ID {
				t.Fatalf("stored object = %+v", obj)
			}

			rc, _, err := svc.Open(ctx, obj.Key, nil)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := ffprobe(t, data).codecs()[want[f]]; !ok {
				t.Fatalf("%s output has no %s stream", f, want[f])
			}

			again, err := svc.Resolve(ctx, integrationURL(), f)
			if err != nil {
				t.Fatal(err)
			}
			if again.Stored == nil || again.Stored.Size != obj.Size {
				t.Fatalf("second Resolve is not a cache hit: %+v", again.Stored)
			}
		})
	}
}

func TestIntegrationStreamCancel(t *testing.T) {
	requireFFmpeg(t)
	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(ytdl.New(os.Getenv("YTDLP_COOKIES"), discard), store, Options{WorkDir: t.TempDir()}, discard)
	if err != nil {
		t.Fatal(err)
	}
	src, err := svc.Resolve(context.Background(), integrationURL(), MP3)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var buf bytes.Buffer
	err = svc.Stream(ctx, src, &buf)
	// A 19 s clip may finish inside the window; either outcome is fine as
	// long as bytes flowed and a cancel surfaced as a context error.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("no bytes streamed")
	}
}
