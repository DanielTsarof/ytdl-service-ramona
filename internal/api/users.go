package api

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{3,64}$`)

func validUsername(u string) error {
	if !usernameRe.MatchString(u) {
		return fmt.Errorf("%w: username must be 3-64 characters of letters, digits, '.', '_' or '-'", errBadParam)
	}
	return nil
}

func validEmail(e string) error {
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return fmt.Errorf("%w: email is not a valid address", errBadParam)
	}
	return nil
}

func parseRole(r string) (dbgen.UserRole, error) {
	role := dbgen.UserRole(r)
	if !role.Valid() {
		return "", fmt.Errorf("%w: role must be user or admin", errBadParam)
	}
	return role, nil
}

// usersChanged invalidates cached user reads (users, /me, key listings).
func (s *Server) usersChanged(ctx context.Context) { s.cache.Bump(ctx, scopeUsers) }

func (s *Server) me(c *gin.Context) {
	c.JSON(http.StatusOK, toUser(mustPrincipal(c).User))
}

// myWebhookSecret returns the secret that signs the caller's webhooks. Not
// cached: secrets do not belong in the response cache.
func (s *Server) myWebhookSecret(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"webhook_secret": mustPrincipal(c).User.WebhookSecret})
}

func (s *Server) rotateMyWebhookSecret(c *gin.Context) {
	s.rotateWebhookSecret(c, mustPrincipal(c).User.ID)
}

func (s *Server) rotateUserWebhookSecret(c *gin.Context) {
	id, err := parseID(c, "id")
	if err != nil {
		s.fail(c, err)
		return
	}
	s.rotateWebhookSecret(c, id)
}

func (s *Server) rotateWebhookSecret(c *gin.Context, userID int64) {
	secret, err := db.NewWebhookSecret()
	if err != nil {
		s.fail(c, err)
		return
	}
	got, err := s.db.SetWebhookSecret(c.Request.Context(), dbgen.SetWebhookSecretParams{ID: userID, WebhookSecret: secret})
	if err != nil {
		s.fail(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"webhook_secret": got})
}

func (s *Server) listUsers(c *gin.Context) {
	pg, err := parsePage(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	ctx := c.Request.Context()
	rows, err := s.db.ListUsers(ctx, dbgen.ListUsersParams{Limit: pg.limit(), Offset: pg.offset()})
	if err != nil {
		s.fail(c, err)
		return
	}
	total, err := s.db.CountUsers(ctx)
	if err != nil {
		s.fail(c, err)
		return
	}
	items := make([]userDTO, len(rows))
	for i, u := range rows {
		items[i] = toUser(u)
	}
	c.JSON(http.StatusOK, newPage(items, pg, total))
}

func (s *Server) getUser(c *gin.Context) {
	id, err := parseID(c, "id")
	if err != nil {
		s.fail(c, err)
		return
	}
	u, err := s.db.GetUserByID(c.Request.Context(), id)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toUser(u))
}

type createUserBody struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	// KeyName names the first API key (default "default").
	KeyName string `json:"key_name"`
}

// createUser creates a user and its first API key; the key is shown once.
func (s *Server) createUser(c *gin.Context) {
	var body createUserBody
	if err := c.ShouldBindJSON(&body); err != nil {
		s.fail(c, fmt.Errorf("%w: invalid JSON body: %v", errBadParam, err))
		return
	}
	body.Username, body.Email = strings.TrimSpace(body.Username), strings.TrimSpace(body.Email)
	if body.Role == "" {
		body.Role = string(dbgen.UserRoleUser)
	}
	if body.KeyName == "" {
		body.KeyName = "default"
	}
	role, err := parseRole(body.Role)
	if err == nil {
		err = validUsername(body.Username)
	}
	if err == nil {
		err = validEmail(body.Email)
	}
	if err != nil {
		s.fail(c, err)
		return
	}

	ctx := c.Request.Context()
	var (
		user      dbgen.User
		plaintext string
		key       dbgen.CreateAPIKeyRow
	)
	err = s.db.InTx(ctx, func(q *dbgen.Queries) error {
		var err error
		if user, err = q.CreateUser(ctx, dbgen.CreateUserParams{Username: body.Username, Email: body.Email, Role: role}); err != nil {
			return err
		}
		plaintext, key, err = db.IssueAPIKey(ctx, q, user.ID, body.KeyName)
		return err
	})
	if err != nil {
		s.fail(c, err)
		return
	}
	s.usersChanged(ctx)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"user":    toUser(user),
		"api_key": issuedKeyDTO{apiKeyDTO: toAPIKeyFromCreate(key), Key: plaintext},
	})
}

type updateUserBody struct {
	Username *string `json:"username"`
	Email    *string `json:"email"`
	Role     *string `json:"role"`
}

func (s *Server) updateUser(c *gin.Context) {
	id, err := parseID(c, "id")
	if err != nil {
		s.fail(c, err)
		return
	}
	var body updateUserBody
	if err := c.ShouldBindJSON(&body); err != nil {
		s.fail(c, fmt.Errorf("%w: invalid JSON body: %v", errBadParam, err))
		return
	}
	p := dbgen.UpdateUserParams{ID: id}
	if body.Username != nil {
		u := strings.TrimSpace(*body.Username)
		if err := validUsername(u); err != nil {
			s.fail(c, err)
			return
		}
		p.Username = &u
	}
	if body.Email != nil {
		e := strings.TrimSpace(*body.Email)
		if err := validEmail(e); err != nil {
			s.fail(c, err)
			return
		}
		p.Email = &e
	}
	if body.Role != nil {
		role, err := parseRole(*body.Role)
		if err != nil {
			s.fail(c, err)
			return
		}
		// An admin demoting themselves could leave nobody able to manage
		// the service.
		if id == mustPrincipal(c).User.ID && role != dbgen.UserRoleAdmin {
			abort(c, http.StatusConflict, codeConflict, "you cannot remove your own admin role")
			return
		}
		p.Role = dbgen.NullUserRole{UserRole: role, Valid: true}
	}
	u, err := s.db.UpdateUser(c.Request.Context(), p)
	if err != nil {
		s.fail(c, err)
		return
	}
	s.usersChanged(c.Request.Context())
	c.JSON(http.StatusOK, toUser(u))
}

func (s *Server) deleteUser(c *gin.Context) {
	id, err := parseID(c, "id")
	if err != nil {
		s.fail(c, err)
		return
	}
	if id == mustPrincipal(c).User.ID {
		abort(c, http.StatusConflict, codeConflict, "you cannot delete your own account")
		return
	}
	ctx := c.Request.Context()
	n, err := s.db.DeleteUser(ctx, id)
	if err != nil {
		s.fail(c, err)
		return
	}
	if n == 0 {
		abort(c, http.StatusNotFound, codeNotFound, "not found")
		return
	}
	s.usersChanged(ctx)
	s.cache.Bump(ctx, historyScope(id), "history:all")
	c.Status(http.StatusNoContent)
}
