package api

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

func (s *Server) listVideos(c *gin.Context) {
	pg, err := parsePage(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	rows, err := s.db.ListVideos(ctx, dbgen.ListVideosParams{Limit: pg.limit(), Offset: pg.offset()})
	if err != nil {
		s.fail(c, err)
		return
	}
	total, err := s.db.CountVideos(ctx)
	if err != nil {
		s.fail(c, err)
		return
	}
	items := make([]mediaDTO, len(rows))
	for i, v := range rows {
		items[i] = toVideo(v)
	}
	c.JSON(http.StatusOK, newPage(items, pg, total))
}

func (s *Server) getVideo(c *gin.Context) {
	v, err := s.db.GetVideoBySourceID(c.Request.Context(), c.Param("source_id"))
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toVideo(v))
}

// deleteVideo removes the stored file (if any) and the catalog row.
func (s *Server) deleteVideo(c *gin.Context) {
	ctx := c.Request.Context()
	v, err := s.db.GetVideoBySourceID(ctx, c.Param("source_id"))
	if err != nil {
		s.fail(c, err)
		return
	}
	if v.StorageKey != nil {
		if err := s.app.DeleteStored(ctx, *v.StorageKey); err != nil {
			s.fail(c, err)
			return
		}
	}
	if _, err := s.db.DeleteVideo(ctx, v.SourceID); err != nil {
		s.fail(c, err)
		return
	}
	s.cache.Bump(ctx, scopeMedia)
	c.Status(http.StatusNoContent)
}

func (s *Server) listAudio(c *gin.Context) {
	pg, err := parsePage(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	rows, err := s.db.ListAudio(ctx, dbgen.ListAudioParams{Limit: pg.limit(), Offset: pg.offset()})
	if err != nil {
		s.fail(c, err)
		return
	}
	total, err := s.db.CountAudio(ctx)
	if err != nil {
		s.fail(c, err)
		return
	}
	items := make([]mediaDTO, len(rows))
	for i, a := range rows {
		items[i] = toAudio(a)
	}
	c.JSON(http.StatusOK, newPage(items, pg, total))
}

func audioParams(c *gin.Context) (dbgen.GetAudioParams, error) {
	f := dbgen.AudioFormat(c.Param("format"))
	if !f.Valid() {
		return dbgen.GetAudioParams{}, fmt.Errorf("%w: format must be mp3 or wav", errBadParam)
	}
	return dbgen.GetAudioParams{SourceID: c.Param("source_id"), Format: f}, nil
}

func (s *Server) getAudio(c *gin.Context) {
	p, err := audioParams(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	a, err := s.db.GetAudio(c.Request.Context(), p)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toAudio(a))
}

func (s *Server) deleteAudio(c *gin.Context) {
	p, err := audioParams(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	a, err := s.db.GetAudio(ctx, p)
	if err != nil {
		s.fail(c, err)
		return
	}
	if a.StorageKey != nil {
		if err := s.app.DeleteStored(ctx, *a.StorageKey); err != nil {
			s.fail(c, err)
			return
		}
	}
	if _, err := s.db.DeleteAudio(ctx, dbgen.DeleteAudioParams{SourceID: a.SourceID, Format: a.Format}); err != nil {
		s.fail(c, err)
		return
	}
	s.cache.Bump(ctx, scopeMedia)
	c.Status(http.StatusNoContent)
}
