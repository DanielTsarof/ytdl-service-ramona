package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

// history lists request history with pagination, filters and date sorting.
//
//	?page=&page_size=           pagination (default 1 / 20, max 100)
//	?kind=file|stream|task
//	?status=pending|ok|error
//	?format=mp4|mp3|wav
//	?from=&to=                  created_at range, RFC 3339 or YYYY-MM-DD (to is exclusive)
//	?q=                         substring of the query, URL or title
//	?sort=-created_at|created_at newest first (default) or oldest first
//	?user_id=                   admins only; users always see their own history
func (s *Server) history(c *gin.Context) {
	p := mustPrincipal(c)
	pg, err := parsePage(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	f, err := parseHistoryFilter(c)
	if err != nil {
		s.fail(c, err)
		return
	}

	switch raw := c.Query("user_id"); {
	case !p.IsAdmin():
		if raw != "" && raw != strconv.FormatInt(p.User.ID, 10) {
			abort(c, http.StatusForbidden, codeForbidden, "you can only view your own history")
			return
		}
		f.UserID = &p.User.ID
	case raw != "":
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || uid < 1 {
			s.fail(c, fmt.Errorf("%w: user_id must be a positive integer", errBadParam))
			return
		}
		f.UserID = &uid
	}

	ctx := c.Request.Context()
	f.PageLimit, f.PageOffset = pg.limit(), pg.offset()
	rows, err := s.db.ListHistory(ctx, f)
	if err != nil {
		s.fail(c, err)
		return
	}
	total, err := s.db.CountHistory(ctx, dbgen.CountHistoryParams{
		UserID: f.UserID, Kind: f.Kind, Status: f.Status, Format: f.Format,
		CreatedFrom: f.CreatedFrom, CreatedTo: f.CreatedTo, Search: f.Search,
	})
	if err != nil {
		s.fail(c, err)
		return
	}
	items := make([]historyDTO, len(rows))
	for i, r := range rows {
		items[i] = toHistory(r)
	}
	c.JSON(http.StatusOK, newPage(items, pg, total))
}

func parseHistoryFilter(c *gin.Context) (dbgen.ListHistoryParams, error) {
	var f dbgen.ListHistoryParams
	if v := c.Query("kind"); v != "" {
		k := dbgen.RequestKind(v)
		if !k.Valid() {
			return f, fmt.Errorf("%w: kind must be one of file, stream, task", errBadParam)
		}
		f.Kind = dbgen.NullRequestKind{RequestKind: k, Valid: true}
	}
	if v := c.Query("status"); v != "" {
		st := dbgen.RequestStatus(v)
		if !st.Valid() {
			return f, fmt.Errorf("%w: status must be one of pending, ok, error", errBadParam)
		}
		f.Status = dbgen.NullRequestStatus{RequestStatus: st, Valid: true}
	}
	if v := c.Query("format"); v != "" {
		mf := dbgen.MediaFormat(strings.ToLower(v))
		if !mf.Valid() {
			return f, fmt.Errorf("%w: format must be one of mp4, mp3, wav", errBadParam)
		}
		f.Format = dbgen.NullMediaFormat{MediaFormat: mf, Valid: true}
	}
	var err error
	if f.CreatedFrom, err = parseTime("from", c.Query("from")); err != nil {
		return f, err
	}
	if f.CreatedTo, err = parseTime("to", c.Query("to")); err != nil {
		return f, err
	}
	if f.CreatedFrom != nil && f.CreatedTo != nil && !f.CreatedFrom.Before(*f.CreatedTo) {
		return f, fmt.Errorf("%w: from must be before to", errBadParam)
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		if len(q) > 200 {
			return f, fmt.Errorf("%w: q is too long", errBadParam)
		}
		esc := escapeLike(q)
		f.Search = &esc
	}
	switch c.DefaultQuery("sort", "-created_at") {
	case "-created_at":
		f.SortAsc = false
	case "created_at":
		f.SortAsc = true
	default:
		return f, fmt.Errorf("%w: sort must be created_at or -created_at", errBadParam)
	}
	return f, nil
}
