package api

import (
	"time"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

// Response types. dbgen structs are never serialised directly: User carries
// the webhook secret and api_keys rows carry hashes.

type userDTO struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	Email        string    `json:"email"`
	Role         string    `json:"role"`
	RegisteredAt time.Time `json:"registered_at"`
}

func toUser(u dbgen.User) userDTO {
	return userDTO{ID: u.ID, Username: u.Username, Email: u.Email, Role: string(u.Role), RegisteredAt: u.RegisteredAt}
}

type apiKeyDTO struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Prefix     string     `json:"prefix"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func toAPIKeyFromCreate(k dbgen.CreateAPIKeyRow) apiKeyDTO {
	return apiKeyDTO{ID: k.ID, UserID: k.UserID, Prefix: k.Prefix, Name: k.Name, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt}
}

func toAPIKeyFromList(k dbgen.ListAPIKeysByUserRow) apiKeyDTO {
	return apiKeyDTO{ID: k.ID, UserID: k.UserID, Prefix: k.Prefix, Name: k.Name, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt}
}

// issuedKeyDTO is returned when a key is created or refreshed: the only
// time the plaintext is ever shown.
type issuedKeyDTO struct {
	apiKeyDTO
	Key string `json:"key"`
}

type mediaDTO struct {
	SourceID string `json:"source_id"`
	Format   string `json:"format"`
	// Quality is set for videos only; audio has no quality.
	Quality         string    `json:"quality,omitempty"`
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	DurationSeconds *int32    `json:"duration_seconds"`
	Stored          bool      `json:"stored"`
	StorageKey      *string   `json:"storage_key"`
	LastUploadedAt  time.Time `json:"last_uploaded_at"`
	LastRequestedAt time.Time `json:"last_requested_at"`
	CreatedAt       time.Time `json:"created_at"`
}

func toVideo(v dbgen.Video) mediaDTO {
	return mediaDTO{
		SourceID: v.SourceID, Format: "mp4", Quality: string(v.Quality), URL: v.Url, Title: v.Title, DurationSeconds: v.DurationSeconds,
		Stored: v.StorageKey != nil, StorageKey: v.StorageKey,
		LastUploadedAt: v.LastUploadedAt, LastRequestedAt: v.LastRequestedAt, CreatedAt: v.CreatedAt,
	}
}

func toAudio(a dbgen.Audio) mediaDTO {
	return mediaDTO{
		SourceID: a.SourceID, Format: string(a.Format), URL: a.Url, Title: a.Title, DurationSeconds: a.DurationSeconds,
		Stored: a.StorageKey != nil, StorageKey: a.StorageKey,
		LastUploadedAt: a.LastUploadedAt, LastRequestedAt: a.LastRequestedAt, CreatedAt: a.CreatedAt,
	}
}

type historyDTO struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	Kind       string    `json:"kind"`
	Query      string    `json:"query,omitempty"`
	URL        string    `json:"url,omitempty"`
	Format     string    `json:"format"`
	SourceID   *string   `json:"source_id"`
	Title      string    `json:"title,omitempty"`
	Status     string    `json:"status"`
	HTTPStatus *int32    `json:"http_status"`
	FromCache  bool      `json:"from_cache"`
	TaskID     *string   `json:"task_id"`
	Error      *string   `json:"error"`
	DurationMs *int64    `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

func toHistory(h dbgen.RequestHistory) historyDTO {
	d := historyDTO{
		ID: h.ID, UserID: h.UserID, Kind: string(h.Kind), Query: h.Query, URL: h.SourceUrl, Format: string(h.Format),
		SourceID: h.SourceID, Title: h.Title, Status: string(h.Status), HTTPStatus: h.HttpStatus, FromCache: h.FromCache,
		Error: h.Error, DurationMs: h.DurationMs, CreatedAt: h.CreatedAt,
	}
	if h.TaskID != nil {
		s := h.TaskID.String()
		d.TaskID = &s
	}
	return d
}

type taskDTO struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	Format        string     `json:"format"`
	Quality       string     `json:"quality"`
	Query         string     `json:"query,omitempty"`
	URL           string     `json:"url,omitempty"`
	SourceID      *string    `json:"source_id"`
	Title         string     `json:"title,omitempty"`
	Error         *string    `json:"error"`
	WebhookURL    *string    `json:"webhook_url"`
	WebhookState  string     `json:"webhook_state"`
	WebhookError  *string    `json:"webhook_error"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	StatusURL     string     `json:"status_url"`
	FileURL       string     `json:"file_url,omitempty"`
	FileAvailable bool       `json:"file_available"`
}

func toTask(t dbgen.Task, now time.Time) taskDTO {
	d := taskDTO{
		ID: t.ID.String(), Status: string(t.Status), Format: string(t.Format), Quality: string(t.Quality), Query: t.Query, URL: t.SourceUrl,
		SourceID: t.SourceID, Title: t.Title, Error: t.Error, WebhookURL: t.WebhookUrl,
		WebhookState: string(t.WebhookState), WebhookError: t.WebhookError,
		CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt, ExpiresAt: t.ExpiresAt,
		StatusURL: "/v1/tasks/" + t.ID.String(),
	}
	if t.Status == dbgen.TaskStatusSucceeded {
		d.FileURL = d.StatusURL + "/file"
		d.FileAvailable = t.ExpiresAt != nil && now.Before(*t.ExpiresAt)
	}
	return d
}
