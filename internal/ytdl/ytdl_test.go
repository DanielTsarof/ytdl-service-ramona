package ytdl

import (
	"os"
	"path/filepath"
	"testing"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

func ptr[T any](v T) *T { return &v }

func TestInfoFromMergedFormats(t *testing.T) {
	raw := &ytdlp.ExtractedInfo{
		ID:         "abc",
		Title:      ptr("Title"),
		WebpageURL: ptr("https://www.youtube.com/watch?v=abc"),
		Duration:   ptr(61.7),
		RequestedFormats: []*ytdlp.ExtractedFormat{
			{URL: "https://v.example/video", VCodec: ptr("avc1.640028"), ACodec: ptr("none")},
			{URL: "https://v.example/audio", VCodec: ptr("none"), ACodec: ptr("mp4a.40.2")},
		},
	}
	info, err := infoFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Inputs) != 2 || info.Inputs[0] != "https://v.example/video" || info.Inputs[1] != "https://v.example/audio" {
		t.Errorf("Inputs = %v", info.Inputs)
	}
	if info.VideoCodec != "avc1.640028" {
		t.Errorf("VideoCodec = %q", info.VideoCodec)
	}
	if info.Duration != 61 || info.Title != "Title" || info.ID != "abc" {
		t.Errorf("info = %+v", info)
	}
}

func TestInfoFromSingleFormat(t *testing.T) {
	raw := &ytdlp.ExtractedInfo{ID: "abc", URL: ptr("https://v.example/audio")}
	info, err := infoFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Inputs) != 1 || info.Inputs[0] != "https://v.example/audio" || info.VideoCodec != "" {
		t.Errorf("info = %+v", info)
	}
}

func TestInfoFromErrors(t *testing.T) {
	if _, err := infoFrom(&ytdlp.ExtractedInfo{ID: "abc"}); err == nil {
		t.Error("no stream URL: expected error")
	}
	if _, err := infoFrom(&ytdlp.ExtractedInfo{URL: ptr("https://x")}); err == nil {
		t.Error("no ID: expected error")
	}
}

func TestFinishedFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"abc.mp4", "abc.f137.mp4", "abc.f140.m4a", "abc.mp4.part", "abc.temp.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := finishedFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "abc.mp4" {
		t.Errorf("got %s", got)
	}

	if _, err := finishedFile(t.TempDir()); err == nil {
		t.Error("empty dir: expected error")
	}
}

func TestIsIntermediate(t *testing.T) {
	for name, want := range map[string]bool{
		"abc.f137.mp4":      true,
		"abc.f251-drc.webm": false,
		"abc.f22-1.mp4":     true,
		"abc.mp4":           false,
		"my.film.mp4":       false,
	} {
		if got := isIntermediate(name); got != want {
			t.Errorf("isIntermediate(%q) = %v, want %v", name, got, want)
		}
	}
}
