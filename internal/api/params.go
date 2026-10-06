package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type page struct {
	Page, Size int
}

func (p page) limit() int32  { return int32(p.Size) }
func (p page) offset() int32 { return int32((p.Page - 1) * p.Size) }

// parsePage reads ?page=&page_size= (1-based, size capped at maxPageSize).
func parsePage(c *gin.Context) (page, error) {
	p := page{Page: 1, Size: defaultPageSize}
	if v := c.Query("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1_000_000 {
			return p, fmt.Errorf("%w: page must be a positive integer", errBadParam)
		}
		p.Page = n
	}
	if v := c.Query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			return p, fmt.Errorf("%w: page_size must be between 1 and %d", errBadParam, maxPageSize)
		}
		p.Size = n
	}
	return p, nil
}

type pageBody[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

func newPage[T any](items []T, p page, total int64) pageBody[T] {
	if items == nil {
		items = []T{}
	}
	return pageBody[T]{Items: items, Page: p.Page, PageSize: p.Size, Total: total}
}

// parseTime accepts RFC 3339 or a plain date (YYYY-MM-DD, midnight UTC).
func parseTime(name, v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("%w: %s must be RFC 3339 or YYYY-MM-DD", errBadParam, name)
}

// escapeLike makes user text literal inside an ILIKE pattern.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func parseID(c *gin.Context, name string) (int64, error) {
	n, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: %s must be a positive integer", errBadParam, name)
	}
	return n, nil
}
