// Package ytdl wraps yt-dlp: resolving a URL or search query to media info and
// direct stream URLs, and downloading finished files.
package ytdl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	ytdlp "github.com/lrstanley/go-ytdlp"
)

// Kind selects which streams yt-dlp picks.
type Kind int

const (
	Audio Kind = iota
	Video
)

const (
	// audioSelector prefers an audio-only stream and falls back to a muxed one
	// (ffmpeg drops the video).
	audioSelector = "bestaudio/best"
	// videoSelector prefers MP4-native streams (H.264 + AAC) so muxing into MP4
	// is a copy, not a re-encode, and falls back to whatever is best.
	videoSelector = "bv*[ext=mp4]+ba[ext=m4a]/b[ext=mp4]/bv*+ba/b"
)

func (k Kind) selector() string {
	if k == Video {
		return videoSelector
	}
	return audioSelector
}

// Client runs yt-dlp. The zero value is not usable; build it with New.
type Client struct {
	cookies string
	log     *slog.Logger
}

// New returns a client. cookies, when non-empty, is a Netscape-format
// cookies.txt passed to every yt-dlp invocation (--cookies).
func New(cookies string, logger *slog.Logger) *Client {
	return &Client{cookies: cookies, log: logger}
}

// newCommand returns the base yt-dlp command with the flags shared by all calls.
func (c *Client) newCommand() *ytdlp.Command {
	cmd := ytdlp.New().
		ForceIPv4().
		SocketTimeout(15).
		Retries("3").
		ExtractorRetries("3").
		NoUpdate().
		NoPlaylist()
	if c.cookies != "" {
		cmd = cmd.Cookies(c.cookies)
	}
	return cmd
}

// Info describes a resolved video.
type Info struct {
	ID         string
	Title      string
	WebpageURL string
	Duration   int // seconds, 0 = unknown
	IsLive     bool
	// Inputs are direct stream URLs for ffmpeg: one muxed/audio stream, or a
	// video stream followed by an audio stream. They are IP-locked and expire
	// within hours, so resolve right before use.
	Inputs []string
	// VideoCodec is the codec of the selected video stream (e.g. "avc1.640028"),
	// empty for audio-only selections.
	VideoCodec string
}

// Resolve returns info for a URL or plain-text search query. Non-URL queries
// are prefixed with "ytsearch1:". Transient network failures (TLS resets
// under throttling) are retried once on top of yt-dlp's own --retries.
func (c *Client) Resolve(ctx context.Context, query string, kind Kind) (Info, error) {
	input := query
	if !isURL(query) {
		input = "ytsearch1:" + query
	}

	start := time.Now()
	const attempts = 2
	for i := 1; ; i++ {
		info, err := c.resolveOnce(ctx, input, kind)
		if err == nil {
			c.log.Debug("yt-dlp resolved",
				slog.String("id", info.ID),
				slog.String("title", info.Title),
				slog.Int("inputs", len(info.Inputs)),
				slog.Duration("elapsed", time.Since(start)))
			return info, nil
		}
		if i >= attempts || ctx.Err() != nil {
			return Info{}, err
		}
		c.log.Debug("yt-dlp resolve failed, retrying", slog.Int("attempt", i), slog.Any("err", err))
		select {
		case <-ctx.Done():
			return Info{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) resolveOnce(ctx context.Context, input string, kind Kind) (Info, error) {
	result, err := c.newCommand().
		Format(kind.selector()).
		DumpJSON().
		Run(ctx, input)
	if err != nil {
		return Info{}, fmt.Errorf("yt-dlp: %w", err)
	}
	infos, err := result.GetExtractedInfo()
	if err != nil {
		return Info{}, fmt.Errorf("parse yt-dlp output: %w", err)
	}
	if len(infos) == 0 {
		return Info{}, errors.New("yt-dlp returned no results")
	}
	return infoFrom(infos[0])
}

// infoFrom converts yt-dlp's extracted info. A merged selection ("bv+ba")
// lists its streams in RequestedFormats; a single-stream selection carries
// the URL at the top level or in the embedded format.
func infoFrom(raw *ytdlp.ExtractedInfo) (Info, error) {
	out := Info{ID: raw.ID}
	if raw.Title != nil {
		out.Title = *raw.Title
	}
	if raw.WebpageURL != nil {
		out.WebpageURL = *raw.WebpageURL
	}
	if raw.Duration != nil {
		out.Duration = int(*raw.Duration)
	}
	if raw.IsLive != nil {
		out.IsLive = *raw.IsLive
	}
	if out.ID == "" {
		return Info{}, errors.New("yt-dlp returned no video ID")
	}

	switch {
	case len(raw.RequestedFormats) > 0:
		for _, f := range raw.RequestedFormats {
			if f == nil || f.URL == "" {
				return Info{}, errors.New("yt-dlp returned a requested format without URL")
			}
			out.Inputs = append(out.Inputs, f.URL)
			if vc := codec(f.VCodec); vc != "" && out.VideoCodec == "" {
				out.VideoCodec = vc
			}
		}
	case raw.URL != nil && *raw.URL != "":
		out.Inputs = []string{*raw.URL}
		if raw.ExtractedFormat != nil {
			out.VideoCodec = codec(raw.ExtractedFormat.VCodec)
		}
	case raw.ExtractedFormat != nil && raw.ExtractedFormat.URL != "":
		out.Inputs = []string{raw.ExtractedFormat.URL}
		out.VideoCodec = codec(raw.ExtractedFormat.VCodec)
	default:
		return Info{}, errors.New("yt-dlp returned no stream URL")
	}
	return out, nil
}

// codec normalises yt-dlp's codec field, where "none" means "no such stream".
func codec(p *string) string {
	if p == nil || *p == "none" {
		return ""
	}
	return *p
}

// Target describes the finished file Download produces.
type Target struct {
	Kind Kind
	// AudioFormat, for Kind == Audio, is the yt-dlp --audio-format ("mp3", "wav").
	AudioFormat string
	// MergeFormat, for Kind == Video, is the yt-dlp --merge-output-format ("mp4").
	MergeFormat string
}

// Download fetches url (use Info.WebpageURL from Resolve) into dir, which
// must be empty and exclusive to this call, and returns the finished file.
// yt-dlp handles throttling and resumption better than ffmpeg reading the raw
// stream URL, and runs ffmpeg itself for extraction and merging.
func (c *Client) Download(ctx context.Context, url string, t Target, dir string) (string, error) {
	cmd := c.newCommand().
		Format(t.Kind.selector()).
		Paths(dir).
		Output("%(id)s.%(ext)s").
		NoProgress()
	switch t.Kind {
	case Audio:
		cmd = cmd.ExtractAudio().AudioFormat(t.AudioFormat).AudioQuality("0")
	case Video:
		cmd = cmd.MergeOutputFormat(t.MergeFormat)
	}

	start := time.Now()
	if _, err := cmd.Run(ctx, url); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("yt-dlp download: %w", err)
	}

	path, err := finishedFile(dir)
	if err != nil {
		return "", err
	}
	c.log.Debug("yt-dlp download finished",
		slog.String("file", filepath.Base(path)),
		slog.Duration("elapsed", time.Since(start)))
	return path, nil
}

// finishedFile finds the one output file in dir, ignoring yt-dlp leftovers
// (.part, .ytdl, intermediate streams yt-dlp failed to clean up).
func finishedFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") ||
			strings.Contains(name, ".temp.") || isIntermediate(name) {
			continue
		}
		found = append(found, filepath.Join(dir, name))
	}
	if len(found) != 1 {
		return "", fmt.Errorf("yt-dlp download: expected 1 output file, found %d", len(found))
	}
	return found[0], nil
}

// isIntermediate matches the per-stream files of a merge ("<id>.f137.mp4").
func isIntermediate(name string) bool {
	parts := strings.Split(name, ".")
	if len(parts) < 3 {
		return false
	}
	f := parts[len(parts)-2]
	return len(f) > 1 && f[0] == 'f' && strings.Trim(f[1:], "0123456789-") == ""
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
