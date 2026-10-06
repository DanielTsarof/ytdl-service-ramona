package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// objectReader adapts a stored object to io.ReadSeeker for
// http.ServeContent, which then implements Range (206/416), If-Range,
// If-None-Match and HEAD. Seeking only records the offset; the next Read
// opens the object from there with a ranged read, so seeking never downloads
// skipped bytes.
type objectReader struct {
	ctx   context.Context
	store storage.Storage
	key   string
	size  int64
	off   int64
	rc    io.ReadCloser
}

func (o *objectReader) Read(p []byte) (int, error) {
	if o.off >= o.size {
		return 0, io.EOF
	}
	if o.rc == nil {
		rc, _, err := o.store.Open(o.ctx, o.key, &storage.ByteRange{Start: o.off, End: -1})
		if err != nil {
			return 0, err
		}
		o.rc = rc
	}
	n, err := o.rc.Read(p)
	o.off += int64(n)
	return n, err
}

func (o *objectReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = o.off + offset
	case io.SeekEnd:
		abs = o.size + offset
	default:
		return 0, errors.New("objectReader: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("objectReader: negative position")
	}
	if abs != o.off && o.rc != nil {
		o.rc.Close()
		o.rc = nil
	}
	o.off = abs
	return abs, nil
}

func (o *objectReader) Close() error {
	if o.rc != nil {
		return o.rc.Close()
	}
	return nil
}

// etag identifies one stored version of an object.
func etag(obj storage.ObjectInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", obj.Key, obj.Size, obj.ModTime.UnixNano())))
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// serveObject writes a stored file with download headers, honouring Range
// and conditional requests.
func (s *Server) serveObject(c *gin.Context, obj storage.ObjectInfo, f media.Format, title, sourceID string, inline bool) {
	ct := obj.ContentType
	if ct == "" {
		ct = f.MIME()
	}
	h := c.Writer.Header()
	h.Set("Content-Type", ct)
	h.Set("ETag", etag(obj))
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Content-Disposition", contentDisposition(inline, filename(title, sourceID, f)))
	h.Set("X-Ytdl-Source-Id", sourceID)

	r := &objectReader{ctx: c.Request.Context(), store: s.app.Store, key: obj.Key, size: obj.Size}
	defer r.Close()
	http.ServeContent(c.Writer, c.Request, "", obj.ModTime, r)
}

// filename builds a safe download name: the title with path separators and
// control characters removed, falling back to the video ID.
func filename(title, sourceID string, f media.Format) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f, strings.ContainsRune(`/\:*?"<>|`, r):
			return -1
		}
		return r
	}, title)
	name = strings.TrimSpace(name)
	if name == "" {
		name = sourceID
	}
	if len(name) > 150 {
		name = strings.ToValidUTF8(name[:150], "")
	}
	return name + "." + f.Ext()
}

// contentDisposition encodes the filename; mime.FormatMediaType emits the
// RFC 2231 UTF-8 form for non-ASCII titles. The ASCII fallback only applies
// if a name still cannot be encoded.
func contentDisposition(inline bool, name string) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	ascii := strings.Map(func(r rune) rune {
		if r > 0x7e || r < 0x20 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return kind + `; filename="` + ascii + `"`
}

// flushWriter pushes each chunk of a live stream to the client immediately
// and records whether anything was written (after the first byte, errors
// can no longer be reported as JSON).
type flushWriter struct {
	w       gin.ResponseWriter
	written bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.written = true
	n, err := f.w.Write(p)
	f.w.Flush()
	return n, err
}
