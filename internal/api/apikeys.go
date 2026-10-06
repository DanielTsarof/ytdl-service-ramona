package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

// userIDParam parses :id and checks the user exists (404 otherwise).
func (s *Server) userIDParam(c *gin.Context) (int64, bool) {
	id, err := parseID(c, "id")
	if err != nil {
		s.fail(c, err)
		return 0, false
	}
	if _, err := s.db.GetUserByID(c.Request.Context(), id); err != nil {
		s.fail(c, err)
		return 0, false
	}
	return id, true
}

func (s *Server) listAPIKeys(c *gin.Context) {
	uid, ok := s.userIDParam(c)
	if !ok {
		return
	}
	rows, err := s.db.ListAPIKeysByUser(c.Request.Context(), uid)
	if err != nil {
		s.fail(c, err)
		return
	}
	items := make([]apiKeyDTO, len(rows))
	for i, k := range rows {
		items[i] = toAPIKeyFromList(k)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (s *Server) createAPIKey(c *gin.Context) {
	uid, ok := s.userIDParam(c)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	// The body is optional.
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			s.fail(c, errors.Join(errBadParam, err))
			return
		}
	}
	name := strings.TrimSpace(body.Name)
	if len(name) > 100 {
		abort(c, http.StatusBadRequest, codeBadRequest, "name is too long")
		return
	}
	ctx := c.Request.Context()
	plaintext, key, err := db.IssueAPIKey(ctx, s.db.Queries, uid, name)
	if err != nil {
		s.fail(c, err)
		return
	}
	s.usersChanged(ctx)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, issuedKeyDTO{apiKeyDTO: toAPIKeyFromCreate(key), Key: plaintext})
}

// revokeAPIKey invalidates one key immediately.
func (s *Server) revokeAPIKey(c *gin.Context) {
	uid, ok := s.userIDParam(c)
	if !ok {
		return
	}
	keyID, err := parseID(c, "key_id")
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	n, err := s.db.RevokeAPIKey(ctx, dbgen.RevokeAPIKeyParams{ID: keyID, UserID: uid})
	if err != nil {
		s.fail(c, err)
		return
	}
	if n == 0 {
		abort(c, http.StatusNotFound, codeNotFound, "no active key with that id for this user")
		return
	}
	s.usersChanged(ctx)
	c.Status(http.StatusNoContent)
}

// refreshAPIKey revokes a key and issues its replacement atomically.
func (s *Server) refreshAPIKey(c *gin.Context) {
	uid, ok := s.userIDParam(c)
	if !ok {
		return
	}
	keyID, err := parseID(c, "key_id")
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	plaintext, key, err := s.db.RefreshAPIKey(ctx, uid, keyID)
	if err != nil {
		s.fail(c, err)
		return
	}
	s.usersChanged(ctx)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, issuedKeyDTO{apiKeyDTO: toAPIKeyFromCreate(key), Key: plaintext})
}

func (s *Server) revokeAllAPIKeys(c *gin.Context) {
	uid, ok := s.userIDParam(c)
	if !ok {
		return
	}
	if uid == mustPrincipal(c).User.ID {
		abort(c, http.StatusConflict, codeConflict, "you cannot revoke all of your own keys")
		return
	}
	ctx := c.Request.Context()
	n, err := s.db.RevokeAllAPIKeysForUser(ctx, uid)
	if err != nil {
		s.fail(c, err)
		return
	}
	s.usersChanged(ctx)
	c.JSON(http.StatusOK, gin.H{"revoked": n})
}
