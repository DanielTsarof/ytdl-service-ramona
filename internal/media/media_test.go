package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/ffmpeg"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/local"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
}

// genInput renders a short synthetic clip with ffmpeg's lavfi sources.
// video/audio select which streams it contains; vcodec picks the video codec.
func genInput(t *testing.T, name string, video, audio bool, vcodec string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if video {
		args = append(args, "-f", "lavfi", "-i", "testsrc=size=160x120:rate=25:duration=2")
	}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=2")
	}
	if video {
		args = append(args, "-c:v", vcodec)
	}
	if audio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generating %s: %v\n%s", name, err, b)
	}
	return out
}

type probe struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
}

func ffprobe(t *testing.T, data []byte) probe {
	t.Helper()
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", "-show_format", "-i", "pipe:0")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var p probe
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("ffprobe json: %v", err)
	}
	return p
}

func (p probe) codecs() map[string]string {
	m := map[string]string{}
	for _, s := range p.Streams {
		m[s.CodecType] = s.CodecName
	}
	return m
}

func liveTranscode(t *testing.T, f Format, info ytdl.Info) []byte {
	t.Helper()
	proc, err := ffmpeg.Start(context.Background(), discard, ffmpeg.Spec{
		Inputs: info.Inputs, OutArgs: f.liveArgs(info), Output: ffmpeg.Pipe,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, readErr := io.ReadAll(proc.Stdout)
	if err := proc.Wait(); err != nil {
		t.Fatalf("ffmpeg %s: %v", f, err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return out
}

func TestLiveArgsProduceValidFiles(t *testing.T) {
	requireFFmpeg(t)
	muxedH264 := genInput(t, "muxed.mp4", true, true, "libx264")
	videoOnly := genInput(t, "video.mp4", true, false, "libx264")
	audioOnly := genInput(t, "audio.m4a", false, true, "")
	muxedMPEG4 := genInput(t, "mpeg4.mp4", true, true, "mpeg4")

	cases := []struct {
		name   string
		f      Format
		info   ytdl.Info
		format string // substring of ffprobe format_name
		want   map[string]string
	}{
		{"mp4 single h264 input", MP4, ytdl.Info{Inputs: []string{muxedH264}, VideoCodec: "avc1.64000d"}, "mp4", map[string]string{"video": "h264", "audio": "aac"}},
		{"mp4 separate video+audio", MP4, ytdl.Info{Inputs: []string{videoOnly, audioOnly}, VideoCodec: "avc1.64000d"}, "mp4", map[string]string{"video": "h264", "audio": "aac"}},
		{"mp4 non-h264 re-encoded", MP4, ytdl.Info{Inputs: []string{muxedMPEG4}, VideoCodec: "mp4v.20.9"}, "mp4", map[string]string{"video": "h264", "audio": "aac"}},
		{"mp3", MP3, ytdl.Info{Inputs: []string{muxedH264}}, "mp3", map[string]string{"audio": "mp3"}},
		{"wav", WAV, ytdl.Info{Inputs: []string{audioOnly}}, "wav", map[string]string{"audio": "pcm_s16le"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ffprobe(t, liveTranscode(t, tc.f, tc.info))
			if !strings.Contains(p.Format.FormatName, tc.format) {
				t.Errorf("container = %q, want %q", p.Format.FormatName, tc.format)
			}
			got := p.codecs()
			for typ, codec := range tc.want {
				if got[typ] != codec {
					t.Errorf("%s codec = %q, want %q (all: %v)", typ, got[typ], codec, got)
				}
			}
			if _, ok := got["video"]; ok && tc.want["video"] == "" {
				t.Errorf("unexpected video stream in %s output", tc.f)
			}
		})
	}
}

// Stored MP4s must be H.264 + AAC with moov before mdat, whatever was
// downloaded: AV1/VP9 files do not play in many players (GStreamer-based
// ones in particular).
func TestStoreArgsProduceValidFiles(t *testing.T) {
	requireFFmpeg(t)
	svc, _ := newService(t)
	cases := []struct {
		name         string
		input        string
		codec        string
		wantReencode bool
	}{
		{"h264 remuxed", genInput(t, "h264.mp4", true, true, "libx264"), "avc1.64001f", false},
		{"non-h264 re-encoded", genInput(t, "mpeg4.mp4", true, true, "mpeg4"), "av01.0.08M.08", true},
		{"unknown codec re-encoded", genInput(t, "unknown.mp4", true, true, "mpeg4"), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, reencode := storeArgs(ytdl.Info{VideoCodec: tc.codec}); reencode != tc.wantReencode {
				t.Errorf("reencode = %v, want %v", reencode, tc.wantReencode)
			}
			out, err := svc.finalizeMP4(context.Background(), discard, ytdl.Info{VideoCodec: tc.codec}, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			p := ffprobe(t, data)
			if got := p.codecs(); got["video"] != "h264" || got["audio"] != "aac" {
				t.Errorf("codecs = %v, want h264 + aac", got)
			}
			moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat"))
			if moov < 0 || mdat < 0 || moov > mdat {
				t.Errorf("moov at %d, mdat at %d: want moov first (faststart)", moov, mdat)
			}
		})
	}
}

func newService(t *testing.T) (*Service, storage.Storage) {
	t.Helper()
	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(ytdl.New("", discard), store, Options{WorkDir: t.TempDir()}, discard)
	if err != nil {
		t.Fatal(err)
	}
	return svc, store
}

func TestStreamLive(t *testing.T) {
	requireFFmpeg(t)
	svc, _ := newService(t)
	in := genInput(t, "muxed.mp4", true, true, "libx264")
	src := Source{Info: ytdl.Info{ID: "x", Inputs: []string{in}}, Format: MP3, Key: Key("x", MP3, QualityBest)}

	var buf bytes.Buffer
	if err := svc.Stream(context.Background(), src, &buf); err != nil {
		t.Fatal(err)
	}
	if got := ffprobe(t, buf.Bytes()).codecs()["audio"]; got != "mp3" {
		t.Fatalf("audio codec = %q", got)
	}
}

func TestStreamFromStorage(t *testing.T) {
	svc, store := newService(t)
	ctx := context.Background()
	key := Key("x", MP3, QualityBest)
	obj, err := store.Put(ctx, key, strings.NewReader("stored bytes"), storage.ObjectInfo{ContentType: MP3.MIME()})
	if err != nil {
		t.Fatal(err)
	}
	// No inputs: a live transcode would fail, so success proves storage was used.
	src := Source{Info: ytdl.Info{ID: "x"}, Format: MP3, Key: key, Stored: &obj}
	var buf bytes.Buffer
	if err := svc.Stream(ctx, src, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "stored bytes" {
		t.Fatalf("got %q", buf.String())
	}
}

// cancelWriter cancels the stream after the first chunk, like a client
// closing the connection mid-playback.
type cancelWriter struct {
	cancel context.CancelFunc
	n      int
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	w.cancel()
	return len(p), nil
}

func TestStreamCancelStopsFFmpeg(t *testing.T) {
	requireFFmpeg(t)
	svc, _ := newService(t)
	// Long enough that ffmpeg is still running when the writer cancels.
	in := filepath.Join(t.TempDir(), "long.wav")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "sine=duration=120", in).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelWriter{cancel: cancel}
	src := Source{Info: ytdl.Info{ID: "x", Inputs: []string{in}}, Format: WAV, Key: Key("x", WAV, QualityBest)}

	err := svc.Stream(ctx, src, w)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if w.n == 0 {
		t.Fatal("nothing written before cancel")
	}
}

func TestStreamMissingInputFails(t *testing.T) {
	requireFFmpeg(t)
	svc, _ := newService(t)
	// No WebpageURL, so the retry reuses the same input instead of calling
	// yt-dlp, and both attempts fail before the first byte.
	src := Source{Info: ytdl.Info{ID: "x", Inputs: []string{filepath.Join(t.TempDir(), "missing.mp4")}}, Format: MP3, Key: Key("x", MP3, QualityBest)}
	if err := svc.Stream(context.Background(), src, io.Discard); err == nil {
		t.Fatal("expected error for missing input")
	}
}

func TestFetchReturnsStoredWithoutDownloading(t *testing.T) {
	svc, _ := newService(t)
	obj := storage.ObjectInfo{Key: "media/x/mp3.mp3", Size: 3}
	got, err := svc.Fetch(context.Background(), Source{Stored: &obj})
	if err != nil || got.Object.Key != obj.Key || got.Downloaded {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLookup(t *testing.T) {
	svc, store := newService(t)
	ctx := context.Background()
	if _, ok, err := svc.Lookup(ctx, "x", MP3, QualityBest); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}
	_, err := store.Put(ctx, Key("x", MP3, QualityBest), strings.NewReader("data"), storage.ObjectInfo{
		ContentType: MP3.MIME(),
		Meta:        map[string]string{"title": "T", "source-url": "https://youtu.be/x", "duration": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	src, ok, err := svc.Lookup(ctx, "x", MP3, QualityBest)
	if err != nil || !ok || src.Stored == nil {
		t.Fatalf("stored file: ok=%v err=%v", ok, err)
	}
	if src.Info.Title != "T" || src.Info.WebpageURL != "https://youtu.be/x" || src.Info.Duration != 42 || src.Key != Key("x", MP3, QualityBest) {
		t.Fatalf("src = %+v", src)
	}
}

func TestKey(t *testing.T) {
	for id, want := range map[string]string{
		"dQw4w9WgXcQ": "media/dQw4w9WgXcQ/mp3.mp3",
		"a/../b":      "media/a_.._b/mp3.mp3",
		"..":          "media/_../mp3.mp3",
		"":            "media/_/mp3.mp3",
	} {
		got := Key(id, MP3, QualityBest)
		if got != want {
			t.Errorf("Key(%q) = %q, want %q", id, got, want)
		}
		if err := storage.ValidateKey(got); err != nil {
			t.Errorf("Key(%q) = %q is not a valid storage key: %v", id, got, err)
		}
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"mp4", "MP3", " wav "} {
		if _, err := ParseFormat(s); err != nil {
			t.Errorf("ParseFormat(%q): %v", s, err)
		}
	}
	if _, err := ParseFormat("flac"); err == nil {
		t.Error("ParseFormat(flac) succeeded")
	}
}

func TestParseQuality(t *testing.T) {
	cases := []struct {
		in   string
		f    Format
		want Quality
	}{
		{"", MP4, QualityBest},
		{"best", MP4, QualityBest},
		{" 720 ", MP4, Quality720},
		{"1080", MP4, Quality1080},
		{"480", MP4, Quality480},
		{"360", MP4, Quality360},
		// Audio has no quality: normalised so it stays one stored file.
		{"720", MP3, QualityBest},
		{"", WAV, QualityBest},
	}
	for _, tc := range cases {
		got, err := ParseQuality(tc.in, tc.f)
		if err != nil || got != tc.want {
			t.Errorf("ParseQuality(%q, %s) = %q, %v; want %q", tc.in, tc.f, got, err, tc.want)
		}
	}
	for _, bad := range []string{"999", "720p", "4k"} {
		if _, err := ParseQuality(bad, MP4); err == nil {
			t.Errorf("ParseQuality(%q) succeeded", bad)
		}
		// Invalid values are rejected even for audio, which ignores valid ones.
		if _, err := ParseQuality(bad, MP3); err == nil {
			t.Errorf("ParseQuality(%q, mp3) succeeded", bad)
		}
	}
	if Quality720.MaxRes() != 720 || QualityBest.MaxRes() != 0 {
		t.Error("MaxRes mismatch")
	}
}

func TestKeyQuality(t *testing.T) {
	for q, want := range map[Quality]string{
		QualityBest: "media/x/mp4.mp4", // unchanged from before qualities existed
		"":          "media/x/mp4.mp4",
		Quality720:  "media/x/mp4-720.mp4",
		Quality360:  "media/x/mp4-360.mp4",
	} {
		if got := Key("x", MP4, q); got != want {
			t.Errorf("Key(x, mp4, %q) = %q, want %q", q, got, want)
		}
	}
}
