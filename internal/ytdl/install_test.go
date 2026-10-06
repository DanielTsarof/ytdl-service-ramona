package ytdl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func testDownloader(t *testing.T, binary []byte, sums string, failFirst int32) (downloader, *int32) {
	t.Helper()
	var binaryHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rel/SHA2-256SUMS":
			io.WriteString(w, sums)
		case "/rel/yt-dlp_musllinux":
			if atomic.AddInt32(&binaryHits, 1) <= failFirst {
				http.Error(w, "flaky", http.StatusBadGateway)
				return
			}
			w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return downloader{
		client:  srv.Client(),
		baseURL: srv.URL + "/rel/",
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &binaryHits
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestFetchVerified(t *testing.T) {
	bin := []byte("#!/bin/sh\necho yt-dlp\n")
	sums := sumOf([]byte("other")) + "  yt-dlp_linux\n" + sumOf(bin) + "  yt-dlp_musllinux\n"
	d, hits := testDownloader(t, bin, sums, 1) // first attempt gets a 502
	dest := filepath.Join(t.TempDir(), "yt-dlp-2026.01.01")

	if err := d.fetchVerified(context.Background(), "yt-dlp_musllinux", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(bin) {
		t.Fatalf("installed binary: %q, %v", got, err)
	}
	if fi, _ := os.Stat(dest); fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary not executable: %v", fi.Mode())
	}
	if *hits != 2 {
		t.Fatalf("binary requested %d times, want 2 (one retry)", *hits)
	}
}

func TestFetchVerifiedRejectsBadChecksum(t *testing.T) {
	bin := []byte("tampered")
	d, _ := testDownloader(t, bin, sumOf([]byte("genuine"))+"  yt-dlp_musllinux\n", 0)
	dir := t.TempDir()
	dest := filepath.Join(dir, "yt-dlp-2026.01.01")

	err := d.fetchOnce(context.Background(), "yt-dlp_musllinux", dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v, want checksum mismatch", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("files left behind after a bad download: %v", entries)
	}
}

func TestFetchVerifiedMissingSum(t *testing.T) {
	d, _ := testDownloader(t, []byte("x"), sumOf([]byte("x"))+"  yt-dlp_linux\n", 0)
	err := d.fetchOnce(context.Background(), "yt-dlp_musllinux", filepath.Join(t.TempDir(), "yt-dlp"))
	if err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("got %v, want missing-entry error", err)
	}
}
