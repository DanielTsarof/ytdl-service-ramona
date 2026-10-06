package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// upstream marks retrieval failures that are not the caller's fault and not
// a server bug (video unavailable, yt-dlp/network errors) as 502.
func upstream(err error) error {
	switch {
	case errors.Is(err, app.ErrInvalidRequest), errors.Is(err, media.ErrTooLong), errors.Is(err, media.ErrLive),
		errors.Is(err, context.Canceled), errors.Is(err, storage.ErrInvalidRange):
		return err
	}
	return fmt.Errorf("%w: %w", errResolve, err)
}

// getFile returns the file for ?url= or ?name= (url wins) in ?format=.
// It is idempotent: repeating it serves the same stored file, and a file
// already in storage is never downloaded again.
func (s *Server) getFile(c *gin.Context) {
	req, err := app.ParseRequest(c.Query("url"), c.Query("name"), c.Query("format"))
	if err != nil {
		s.fail(c, err)
		return
	}
	h := s.startHistory(c, dbgen.RequestKindFile, req, nil)
	res, err := s.app.Get(c.Request.Context(), req)
	if err != nil {
		err = upstream(err)
		s.finishHistory(c, h, nil, false, err)
		s.fail(c, err)
		return
	}
	s.serveObject(c, res.Object, req.Format, res.Source.Info.Title, res.Source.Info.ID, false)
	s.finishHistory(c, h, &res.Source, res.FromCache, nil)
}

// stream plays ?url= / ?name= in ?format= as it is produced. A stored file
// is served directly (seekable, Range supported); otherwise ffmpeg
// transcodes the source live into the response, so playback starts without
// waiting for a download. Live output is not stored.
func (s *Server) stream(c *gin.Context) {
	req, err := app.ParseRequest(c.Query("url"), c.Query("name"), c.Query("format"))
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	h := s.startHistory(c, dbgen.RequestKindStream, req, nil)

	src, err := s.app.Locate(ctx, req)
	if err != nil {
		err = upstream(err)
		s.finishHistory(c, h, nil, false, err)
		s.fail(c, err)
		return
	}
	if src.Stored != nil {
		s.app.Record(ctx, src, false)
		s.serveObject(c, *src.Stored, req.Format, src.Info.Title, src.Info.ID, true)
		s.finishHistory(c, h, &src, true, nil)
		return
	}

	c.Header("Content-Type", req.Format.MIME())
	c.Header("Cache-Control", "no-store")
	c.Header("X-Ytdl-Source-Id", src.Info.ID)
	c.Header("Content-Disposition", contentDisposition(true, filename(src.Info.Title, src.Info.ID, req.Format)))
	c.Status(http.StatusOK)
	fw := &flushWriter{w: c.Writer}
	err = s.app.Media.Stream(ctx, src, fw)
	switch {
	case err == nil:
		s.finishHistory(c, h, &src, false, nil)
	case !fw.written:
		err = upstream(err)
		s.finishHistory(c, h, &src, false, err)
		s.fail(c, err)
	default:
		// Headers and part of the body are out: the client sees a truncated
		// stream, and the failure is recorded here.
		s.log.Warn("live stream failed mid-response",
			slog.String("request_id", c.GetString(ctxRequestID)), slog.Any("err", err))
		s.finishHistory(c, h, &src, false, upstream(err))
	}
}

// history records are best effort: a failure to log a request must not fail
// the request.
type historyRec struct {
	id     int64
	userID int64
}

func (s *Server) startHistory(c *gin.Context, kind dbgen.RequestKind, req app.Request, taskID *uuid.UUID) *historyRec {
	p := mustPrincipal(c)
	keyID := p.KeyID
	id, err := s.db.InsertHistory(c.Request.Context(), dbgen.InsertHistoryParams{
		UserID: p.User.ID, ApiKeyID: &keyID, Kind: kind,
		Query: req.Name, SourceUrl: req.URL, Format: dbgen.MediaFormat(req.Format), TaskID: taskID,
	})
	if err != nil {
		s.log.Warn("recording request history failed", slog.Any("err", err))
		return nil
	}
	return &historyRec{id: id, userID: p.User.ID}
}

func (s *Server) finishHistory(c *gin.Context, h *historyRec, src *media.Source, fromCache bool, reqErr error) {
	if h == nil {
		return
	}
	ctx := context.WithoutCancel(c.Request.Context())
	status := c.Writer.Status()
	p := dbgen.FinishHistoryParams{ID: h.id, Status: dbgen.RequestStatusOk, FromCache: fromCache}
	if reqErr != nil {
		p.Status = dbgen.RequestStatusError
		code, _, msg := classify(reqErr)
		status = code
		p.Error = &msg
	}
	st := int32(status)
	p.HttpStatus = &st
	if src != nil {
		p.SourceID = &src.Info.ID
		p.Title = src.Info.Title
	}
	if err := s.db.FinishHistory(ctx, p); err != nil {
		s.log.Warn("updating request history failed", slog.Any("err", err))
		return
	}
	s.cache.Bump(ctx, historyScope(h.userID), "history:all")
}
