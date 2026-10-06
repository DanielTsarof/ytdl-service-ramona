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

// Quality caps a video's resolution by the smaller side of the frame, so
// "720" is 1280x720 landscape or 720x1280 portrait. It is a preference, not a
// filter: yt-dlp takes the largest resolution at or below the cap, or the
// smallest above it when nothing smaller exists. Best is uncapped.
type Quality string

const (
	QualityBest Quality = "best"
	Quality1080 Quality = "1080"
	Quality720  Quality = "720"
	Quality480  Quality = "480"
	Quality360  Quality = "360"
)

var Qualities = []Quality{QualityBest, Quality1080, Quality720, Quality480, Quality360}

// ParseQuality validates s for format f. Empty means best, and audio formats
// always get best: quality only applies to video, and normalising it keeps
// audio requests that differ only in quality on one stored file.
func ParseQuality(s string, f Format) (Quality, error) {
	q := Quality(strings.ToLower(strings.TrimSpace(s)))
	switch q {
	case "":
		q = QualityBest
	case QualityBest, Quality1080, Quality720, Quality480, Quality360:
	default:
		return "", fmt.Errorf("unsupported quality %q (want 360, 480, 720, 1080 or best)", s)
	}
	if f != MP4 {
		return QualityBest, nil
	}
	return q, nil
}

// MaxRes is the resolution cap in pixels, 0 for best.
func (q Quality) MaxRes() int {
	switch q {
	case Quality1080:
		return 1080
	case Quality720:
		return 720
	case Quality480:
		return 480
	case Quality360:
		return 360
	}
	return 0
}

// normal maps the zero value to best, so a Source built without a quality
// behaves as before qualities existed.
func (q Quality) normal() Quality {
	if q == "" {
		return QualityBest
	}
	return q
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
func (f Format) target(q Quality) ytdl.Target {
	if f == MP4 {
		return ytdl.Target{Kind: ytdl.Video, MergeFormat: "mp4", MaxRes: q.MaxRes()}
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

// storeArgs are the ffmpeg output args that turn a downloaded MP4 into the
// stored file: H.264 + AAC with the moov atom first (+faststart), so it plays
// and seeks in any player and browser. H.264 sources are remuxed without
// re-encoding the video; anything else (AV1/VP9 fallbacks, other sites, an
// unknown codec) is re-encoded. reencode reports which path was taken.
func storeArgs(info ytdl.Info) (args []string, reencode bool) {
	args = []string{"-map", "0:v:0", "-map", "0:a:0?"}
	if isH264(info.VideoCodec) {
		args = append(args, "-c", "copy")
	} else {
		reencode = true
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "192k")
	}
	return append(args, "-movflags", "+faststart", "-f", "mp4"), reencode
}

func isH264(codec string) bool {
	return strings.HasPrefix(codec, "avc1") || strings.HasPrefix(codec, "h264")
}
