package s3

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/storagetest"
)

// TestConformance runs against a real S3-compatible store, e.g. MinIO:
//
//	docker run -d --rm --name ytdl-s3-test -p 19100:9000 \
//	  -e RUSTFS_ACCESS_KEY=testkey -e RUSTFS_SECRET_KEY=testsecret123 rustfs/rustfs:1.0.1
//	S3_TEST_ENDPOINT=http://localhost:19100 \
//	AWS_ACCESS_KEY_ID=testkey AWS_SECRET_ACCESS_KEY=testsecret123 \
//	go test ./internal/storage/s3/
//
// The bucket is created if missing (CreateBucket), which exercises that too.
func TestConformance(t *testing.T) {
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT not set")
	}
	bucket := os.Getenv("S3_TEST_BUCKET")
	if bucket == "" {
		bucket = "ytdl-test"
	}
	ctx := context.Background()
	s, err := New(ctx, Options{
		Bucket:    bucket,
		Region:    "us-east-1",
		Endpoint:  endpoint,
		PathStyle: true,
		Prefix:    "conformance",

		CreateBucket: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := "run-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	storagetest.Run(t, s, run)

	t.Run("signed url", func(t *testing.T) {
		key := run + "/signed.txt"
		if _, err := s.Put(ctx, key, strings.NewReader("hello"), storage.ObjectInfo{ContentType: "text/plain"}); err != nil {
			t.Fatal(err)
		}
		defer s.Delete(ctx, key)
		u, err := s.SignedURL(ctx, key, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(b) != "hello" {
			t.Fatalf("GET signed URL: %d %q", resp.StatusCode, b)
		}
	})
}

func TestTotalFromContentRange(t *testing.T) {
	for in, want := range map[string]int64{"bytes 0-99/1234": 1234, "bytes 5-5/6": 6} {
		if got, ok := totalFromContentRange(in); !ok || got != want {
			t.Errorf("%q: got %d %v", in, got, ok)
		}
	}
	if _, ok := totalFromContentRange("bytes */*"); ok {
		t.Error("unknown total parsed")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	in := map[string]string{"title": "Привет & ♪ / ?=", "id": "abc"}
	got := decodeMeta(encodeMeta(in))
	for k, v := range in {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	for _, v := range encodeMeta(in) {
		for _, r := range v {
			if r > 127 {
				t.Fatalf("encoded value %q is not ASCII", v)
			}
		}
	}
}
