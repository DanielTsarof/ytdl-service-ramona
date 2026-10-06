package api

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/jobs"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
)

const maxTaskBody = 16 << 10

var idempotencyKeyRe = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)

type createTaskBody struct {
	URL        string `json:"url"`
	Name       string `json:"name"`
	Format     string `json:"format"`
	Quality    string `json:"quality"`
	WebhookURL string `json:"webhook_url"`
}

// requestHash fingerprints the normalised request. It detects an
// Idempotency-Key reused for a different request, and without a key it is
// the implicit idempotency key.
func requestHash(req app.Request, webhook string) []byte {
	sum := sha256.Sum256([]byte(strings.Join([]string{req.URL, req.Name, string(req.Format), string(req.Quality), webhook}, "\x00")))
	return sum[:]
}

// createTask enqueues an asynchronous retrieval. With webhook_url the file
// is POSTed there when ready; without, the client polls the task and
// downloads the result within TASK_RESULT_TTL.
//
// Idempotent: with an Idempotency-Key header a repeat returns the same task
// (200) and reusing the key for a different request is rejected (422).
// Without the header, an identical request whose task is still running or
// whose result is still downloadable returns that task.
func (s *Server) createTask(c *gin.Context) {
	ctx := c.Request.Context()
	p := mustPrincipal(c)

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTaskBody)
	var body createTaskBody
	if err := c.ShouldBindJSON(&body); err != nil {
		s.fail(c, fmt.Errorf("%w: invalid JSON body: %v", errBadParam, err))
		return
	}
	req, err := app.ParseRequest(body.URL, body.Name, body.Format, body.Quality)
	if err != nil {
		s.fail(c, err)
		return
	}
	body.WebhookURL = strings.TrimSpace(body.WebhookURL)
	var webhook *string
	if body.WebhookURL != "" {
		if err := jobs.ValidateWebhookURL(body.WebhookURL, s.cfg.WebhookAllowPrivate); err != nil {
			s.fail(c, err)
			return
		}
		webhook = &body.WebhookURL
	}
	hash := requestHash(req, body.WebhookURL)

	var idemKey *string
	if k := c.GetHeader("Idempotency-Key"); k != "" {
		if !idempotencyKeyRe.MatchString(k) {
			s.fail(c, fmt.Errorf("%w: Idempotency-Key must be 1-255 printable ASCII characters", errBadParam))
			return
		}
		idemKey = &k
		existing, err := s.db.GetTaskByIdempotency(ctx, dbgen.GetTaskByIdempotencyParams{UserID: p.User.ID, IdempotencyKey: idemKey})
		if err == nil {
			s.replayTask(c, existing, hash)
			return
		}
		if !db.IsNotFound(err) {
			s.fail(c, err)
			return
		}
	} else {
		existing, err := s.db.FindReusableTask(ctx, dbgen.FindReusableTaskParams{UserID: p.User.ID, RequestHash: hash})
		if err == nil {
			c.Header("Idempotent-Replayed", "true")
			c.JSON(http.StatusOK, toTask(existing, s.now()))
			return
		}
		if !db.IsNotFound(err) {
			s.fail(c, err)
			return
		}
	}

	keyID := p.KeyID
	task, err := s.db.CreateTask(ctx, dbgen.CreateTaskParams{
		UserID: p.User.ID, ApiKeyID: &keyID, IdempotencyKey: idemKey, RequestHash: hash,
		Query: req.Name, SourceUrl: req.URL, Format: dbgen.MediaFormat(req.Format),
		Quality: app.VideoQuality(req.Quality), WebhookUrl: webhook,
	})
	if db.IsNotFound(err) && idemKey != nil {
		// Lost a race with a concurrent request using the same key.
		existing, gerr := s.db.GetTaskByIdempotency(ctx, dbgen.GetTaskByIdempotencyParams{UserID: p.User.ID, IdempotencyKey: idemKey})
		if gerr != nil {
			s.fail(c, gerr)
			return
		}
		s.replayTask(c, existing, hash)
		return
	}
	if err != nil {
		s.fail(c, err)
		return
	}

	if h := s.startHistory(c, dbgen.RequestKindTask, req, &task.ID); h != nil {
		if err := s.db.SetTaskHistory(ctx, dbgen.SetTaskHistoryParams{ID: task.ID, HistoryID: &h.id}); err != nil {
			s.log.Warn("linking task to history failed", slog.Any("err", err))
		}
		s.cache.Bump(ctx, historyScope(p.User.ID), "history:all")
	}
	s.waker.Wake()

	dto := toTask(task, s.now())
	c.Header("Location", dto.StatusURL)
	c.JSON(http.StatusAccepted, dto)
}

func (s *Server) replayTask(c *gin.Context, t dbgen.Task, hash []byte) {
	if !bytes.Equal(t.RequestHash, hash) {
		abort(c, http.StatusUnprocessableEntity, codeIdempotency,
			"Idempotency-Key was already used for a different request")
		return
	}
	c.Header("Idempotent-Replayed", "true")
	c.JSON(http.StatusOK, toTask(t, s.now()))
}

// loadTask returns the task if the caller owns it or is an admin. Others get
// 404 rather than 403, so task IDs cannot be probed.
func (s *Server) loadTask(c *gin.Context) (dbgen.Task, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		s.fail(c, fmt.Errorf("%w: task id must be a UUID", errBadParam))
		return dbgen.Task{}, false
	}
	t, err := s.db.GetTask(c.Request.Context(), id)
	if err != nil {
		s.fail(c, err)
		return dbgen.Task{}, false
	}
	if p := mustPrincipal(c); t.UserID != p.User.ID && !p.IsAdmin() {
		abort(c, http.StatusNotFound, codeNotFound, "not found")
		return dbgen.Task{}, false
	}
	return t, true
}

func (s *Server) getTask(c *gin.Context) {
	if t, ok := s.loadTask(c); ok {
		c.JSON(http.StatusOK, toTask(t, s.now()))
	}
}

// getTaskFile serves a finished task's file until expires_at.
func (s *Server) getTaskFile(c *gin.Context) {
	t, ok := s.loadTask(c)
	if !ok {
		return
	}
	switch t.Status {
	case dbgen.TaskStatusQueued, dbgen.TaskStatusRunning:
		c.Header("Retry-After", "5")
		abort(c, http.StatusConflict, codeTaskNotReady, "task is "+string(t.Status))
		return
	case dbgen.TaskStatusFailed:
		msg := "task failed"
		if t.Error != nil {
			msg += ": " + *t.Error
		}
		abort(c, http.StatusConflict, codeTaskFailed, msg)
		return
	}
	if t.ExpiresAt == nil || !s.now().Before(*t.ExpiresAt) || t.StorageKey == nil {
		abort(c, http.StatusGone, codeGone, "task result has expired")
		return
	}
	ctx := c.Request.Context()
	obj, err := s.app.Store.Stat(ctx, *t.StorageKey)
	if errors.Is(err, storage.ErrNotFound) {
		abort(c, http.StatusGone, codeGone, "task result is no longer stored")
		return
	}
	if err != nil {
		s.fail(c, err)
		return
	}
	f := media.Format(t.Format)
	sourceID := ""
	if t.SourceID != nil {
		sourceID = *t.SourceID
	}
	src := media.Source{Format: f, Key: *t.StorageKey, Stored: &obj}
	src.Info.ID = sourceID
	s.app.Record(ctx, src, false)
	s.serveObject(c, obj, f, t.Title, sourceID, false)
}
