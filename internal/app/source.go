package app

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
)

// ErrInvalidRequest marks caller mistakes (HTTP 400).
var ErrInvalidRequest = errors.New("invalid request")

const (
	maxURLLen  = 2048
	maxNameLen = 300
)

// Request is a validated retrieval request.
type Request struct {
	// URL wins over Name when both are given.
	URL    string
	Name   string
	Format media.Format
}

// ParseRequest validates raw parameters. url takes priority: when it is
// non-empty, name is ignored.
func ParseRequest(rawURL, name, format string) (Request, error) {
	f, err := media.ParseFormat(format)
	if err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	rawURL, name = strings.TrimSpace(rawURL), strings.TrimSpace(name)
	switch {
	case rawURL != "":
		if len(rawURL) > maxURLLen {
			return Request{}, fmt.Errorf("%w: url longer than %d characters", ErrInvalidRequest, maxURLLen)
		}
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Request{}, fmt.Errorf("%w: url must be an absolute http(s) URL", ErrInvalidRequest)
		}
		return Request{URL: rawURL, Format: f}, nil
	case name != "":
		if len(name) > maxNameLen {
			return Request{}, fmt.Errorf("%w: name longer than %d characters", ErrInvalidRequest, maxNameLen)
		}
		return Request{Name: name, Format: f}, nil
	}
	return Request{}, fmt.Errorf("%w: either url or name is required", ErrInvalidRequest)
}

// Query is what yt-dlp is asked for.
func (r Request) Query() string {
	if r.URL != "" {
		return r.URL
	}
	return r.Name
}

var youtubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// YouTubeID extracts the video ID from the common YouTube URL shapes without
// a network call, so a stored file can be served without running yt-dlp.
// It returns "" for anything it does not recognise; the caller then resolves.
func YouTubeID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	var id string
	switch host {
	case "youtu.be":
		id = strings.Trim(u.Path, "/")
	case "youtube.com", "music.youtube.com", "youtube-nocookie.com":
		switch {
		case u.Path == "/watch":
			id = u.Query().Get("v")
		default:
			for _, prefix := range []string{"/shorts/", "/embed/", "/live/", "/v/"} {
				if rest, ok := strings.CutPrefix(u.Path, prefix); ok {
					id, _, _ = strings.Cut(rest, "/")
					break
				}
			}
		}
	}
	if !youtubeIDRe.MatchString(id) {
		return ""
	}
	return id
}
