package app

import (
	"errors"
	"testing"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
)

func TestParseRequestURLBeatsName(t *testing.T) {
	r, err := ParseRequest("https://youtu.be/dQw4w9WgXcQ", "some song", "mp3", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "https://youtu.be/dQw4w9WgXcQ" || r.Name != "" || r.Query() != r.URL || r.Format != media.MP3 {
		t.Fatalf("got %+v", r)
	}
	r, err = ParseRequest("  ", " some song ", "MP4", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "" || r.Name != "some song" || r.Query() != "some song" || r.Format != media.MP4 || r.Quality != media.QualityBest {
		t.Fatalf("got %+v", r)
	}
	r, err = ParseRequest("https://youtu.be/dQw4w9WgXcQ", "", "mp4", "720")
	if err != nil || r.Quality != media.Quality720 {
		t.Fatalf("mp4 720: %+v, %v", r, err)
	}
	// Audio ignores quality.
	r, err = ParseRequest("https://youtu.be/dQw4w9WgXcQ", "", "mp3", "720")
	if err != nil || r.Quality != media.QualityBest {
		t.Fatalf("mp3 720: %+v, %v", r, err)
	}
}

func TestParseRequestErrors(t *testing.T) {
	for name, tc := range map[string][4]string{
		"neither":      {"", "", "mp3", ""},
		"bad format":   {"https://x.y/z", "", "flac", ""},
		"not absolute": {"youtube.com/watch?v=x", "", "mp3", ""},
		"bad scheme":   {"file:///etc/passwd", "", "mp3", ""},
		"bad quality":  {"https://x.y/z", "", "mp4", "4k"},
	} {
		if _, err := ParseRequest(tc[0], tc[1], tc[2], tc[3]); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: got %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestYouTubeID(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":          "dQw4w9WgXcQ",
		"https://youtube.com/watch?v=dQw4w9WgXcQ&t=42s":        "dQw4w9WgXcQ",
		"https://m.youtube.com/watch?v=dQw4w9WgXcQ":            "dQw4w9WgXcQ",
		"https://music.youtube.com/watch?v=dQw4w9WgXcQ":        "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?si=abc":                  "dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ":           "dQw4w9WgXcQ",
		"https://www.youtube.com/embed/dQw4w9WgXcQ":            "dQw4w9WgXcQ",
		"https://www.youtube.com/live/dQw4w9WgXcQ?feature=x":   "dQw4w9WgXcQ",
		"https://www.youtube.com/playlist?list=PL123":          "",
		"https://www.youtube.com/watch?v=short":                "",
		"https://vimeo.com/123456":                             "",
		"https://evil.example/watch?v=dQw4w9WgXcQ":             "",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ/../../..": "",
	} {
		if got := YouTubeID(in); got != want {
			t.Errorf("YouTubeID(%q) = %q, want %q", in, got, want)
		}
	}
}
