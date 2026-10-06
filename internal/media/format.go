package media

import (
	"fmt"
	"strings"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/ytdl"
)

// Format is an output file format users can request.
type Format string

const (
	MP4 Format = "mp4"
	MP3 Format = "mp3"
	WAV Format = "wav"
)

var Formats = []Format{MP4, MP3, WAV}

func ParseFormat(s string) (Format, error) {
	f := Format(strings.ToLower(strings.TrimSpace(s)))
	switch f {
	case MP4, MP3, WAV:
		return f, nil
	}
	return "", fmt.Errorf("unsupported format %q (want mp4, mp3 or wav)", s)
}

func (f Format) Ext() string { return string(f) }

func (f Format) MIME() string {
	switch f {
	case MP4:
		return "video/mp4"
	case MP3:
		return "audio/mpeg"
	case WAV:
		return "audio/wav"
	}
	return "application/octet-stream"
}

func (f Format) Kind() ytdl.Kind {
	if f == MP4 {
		return ytdl.Video
	}
	return ytdl.Audio
}

// target is what yt-dlp should produce for a stored file.
func (f Format) target() ytdl.Target {
	if f == MP4 {
		return ytdl.Target{Kind: ytdl.Video, MergeFormat: "mp4"}
	}
	return ytdl.Target{Kind: ytdl.Audio, AudioFormat: string(f)}
}

// liveArgs are the ffmpeg output args for transcoding info's inputs into a
// non-seekable pipe. Every container here can be written front to back:
// fragmented MP4 needs no moov rewrite, and ffmpeg writes WAV with a
// streaming-size header when it cannot seek back.
func (f Format) liveArgs(info ytdl.Info) []string {
	switch f {
	case MP4:
		args := []string{"-map", "0:v:0"}
		if len(info.Inputs) > 1 {
			args = append(args, "-map", "1:a:0")
		} else {
			args = append(args, "-map", "0:a:0?")
		}
		// H.264 goes into MP4 as-is; anything else (VP9, AV1 from the
		// fallback selectors) is re-encoded for broad player support.
		if isH264(info.VideoCodec) {
			args = append(args, "-c:v", "copy")
		} else {
			args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p")
		}
		return append(args,
			"-c:a", "aac", "-b:a", "192k",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4")
	case MP3:
		return []string{"-map", "0:a:0", "-vn", "-c:a", "libmp3lame", "-q:a", "2", "-f", "mp3"}
	case WAV:
		return []string{"-map", "0:a:0", "-vn", "-c:a", "pcm_s16le", "-ar", "44100", "-ac", "2", "-f", "wav"}
	}
	return nil
}

func isH264(codec string) bool {
	return strings.HasPrefix(codec, "avc1") || strings.HasPrefix(codec, "h264")
}
