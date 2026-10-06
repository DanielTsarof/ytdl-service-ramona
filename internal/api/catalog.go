package api

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/app"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/media"
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
	q, err := videoQuality(c.Query("quality"))
	if err != nil {
		s.fail(c, err)
		return
	}
	v, err := s.db.GetVideo(c.Request.Context(), dbgen.GetVideoParams{SourceID: c.Param("source_id"), Quality: q})
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toVideo(v))
}

// deleteVideo removes stored files and their catalog rows: one quality with
// ?quality=, otherwise every quality of the video.
func (s *Server) deleteVideo(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("source_id")
	var rows []dbgen.Video
	if raw := c.Query("quality"); raw != "" {
		q, err := videoQuality(raw)
		if err != nil {
			s.fail(c, err)
			return
		}
		v, err := s.db.GetVideo(ctx, dbgen.GetVideoParams{SourceID: id, Quality: q})
		if err != nil {
			s.fail(c, err)
			return
		}
		rows = []dbgen.Video{v}
	} else {
		var err error
		if rows, err = s.db.ListVideosBySourceID(ctx, id); err != nil {
			s.fail(c, err)
			return
		}
		if len(rows) == 0 {
			s.fail(c, pgx.ErrNoRows)
			return
		}
	}
	for _, v := range rows {
		if v.StorageKey != nil {
			if err := s.app.DeleteStored(ctx, *v.StorageKey); err != nil {
				s.fail(c, err)
				return
			}
		}
		if _, err := s.db.DeleteVideo(ctx, dbgen.DeleteVideoParams{SourceID: v.SourceID, Quality: v.Quality}); err != nil {
			s.fail(c, err)
			return
		}
	}
	s.cache.Bump(ctx, scopeMedia)
	c.Status(http.StatusNoContent)
}

// videoQuality parses an optional ?quality= for the video catalog (empty is best).
func videoQuality(raw string) (dbgen.VideoQuality, error) {
	q, err := media.ParseQuality(raw, media.MP4)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errBadParam, err)
	}
	return app.VideoQuality(q), nil
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
