package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/jobs"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

// Error codes returned in {"error":{"code":...}}.
const (
	codeBadRequest     = "bad_request"
	codeUnauthorized   = "unauthorized"
	codeForbidden      = "forbidden"
	codeNotFound       = "not_found"
	codeConflict       = "conflict"
	codeGone           = "gone"
	codeUnprocessable  = "unprocessable"
	codeRateLimited    = "rate_limited"
	codeRangeNotSat    = "range_not_satisfiable"
	codeUpstream       = "upstream_failed"
	codeInternal       = "internal"
	codeIdempotency    = "idempotency_key_reused"
	codeTaskNotReady   = "task_not_ready"
	codeTaskFailed     = "task_failed"
	codeUnavailable    = "unavailable"
	pgUniqueViolation  = "23505"
	pgForeignKeyFailed = "23503"
)

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func abort(c *gin.Context, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	c.AbortWithStatusJSON(status, b)
}

// fail maps an internal error to a response. Unknown errors become a
// generic 500 and are logged with detail; their text never reaches clients.
func (s *Server) fail(c *gin.Context, err error) {
	status, code, msg := classify(err)
	if status >= 500 {
		s.log.Error("request failed",
			slog.String("request_id", c.GetString(ctxRequestID)),
			slog.String("path", c.FullPath()),
			slog.Any("err", err))
	}
	abort(c, status, code, msg)
}

func classify(err error) (int, string, string) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, app.ErrInvalidRequest), errors.Is(err, errBadParam):
		return http.StatusBadRequest, codeBadRequest, err.Error()
	case errors.Is(err, jobs.ErrForbiddenTarget):
		return http.StatusBadRequest, codeBadRequest, err.Error()
	case errors.Is(err, media.ErrTooLong), errors.Is(err, media.ErrLive):
		return http.StatusUnprocessableEntity, codeUnprocessable, err.Error()
	case db.IsNotFound(err), errors.Is(err, storage.ErrNotFound):
		return http.StatusNotFound, codeNotFound, "not found"
	case errors.Is(err, storage.ErrInvalidRange):
		return http.StatusRequestedRangeNotSatisfiable, codeRangeNotSat, "range not satisfiable"
	case errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation:
		return http.StatusConflict, codeConflict, "already exists: " + pgErr.ConstraintName
	case errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyFailed:
		return http.StatusConflict, codeConflict, "referenced row does not exist"
	case errors.Is(err, errResolve):
		return http.StatusBadGateway, codeUpstream, jobs.PublicError(err)
	}
	return http.StatusInternalServerError, codeInternal, "internal error"
}

// errBadParam marks invalid query/body parameters.
var errBadParam = errors.New("invalid parameter")

// errResolve wraps yt-dlp/ffmpeg failures, which are upstream problems
// (video unavailable, network) rather than server bugs.
var errResolve = errors.New("could not retrieve the media")
