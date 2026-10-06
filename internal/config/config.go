package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
	"github.com/jackc/pgx/v5/pgconn"
)

type Config struct {
	// DatabaseURL: PostgreSQL connection string, URL or keyword/value form
	// (postgres://user:pass@host:5432/db?sslmode=disable).
	DatabaseURL string `env:"DATABASE_URL" env-required:"true"`
	// YtdlpCookies: path to a Netscape-format cookies.txt passed to yt-dlp (empty = off).
	YtdlpCookies string `env:"YTDLP_COOKIES"`
	// YtdlpSelfUpdate updates the yt-dlp binary to the latest stable release
	// in the background at startup; YouTube breaks old releases regularly.
	// Turn off to keep a pinned binary.
	YtdlpSelfUpdate bool `env:"YTDLP_SELF_UPDATE" env-default:"true"`
	// WorkDir holds per-job scratch directories while yt-dlp downloads and
	// post-processes; finished files are moved into Storage and the scratch
	// dir is removed. Empty = <os temp>/ytdl-service.
	WorkDir string `env:"WORK_DIR"`
	// MaxDurationSeconds rejects sources longer than this before any download
	// starts (0 = no limit).
	MaxDurationSeconds int `env:"MAX_DURATION_SECONDS" env-default:"10800"`
	// LogLevel: debug | info | warn | error.
	LogLevel string `env:"LOG_LEVEL" env-default:"info"`
	// LogFormat: text (human-readable) | json.
	LogFormat string `env:"LOG_FORMAT" env-default:"text"`

	Storage Storage
	HTTP    HTTP
	Jobs    Jobs
	Limits  Limits
	Admin   BootstrapAdmin
}

type HTTP struct {
	Addr string `env:"HTTP_ADDR" env-default:":8080"`
	// RedisURL backs rate limiting and the read cache.
	RedisURL string `env:"REDIS_URL" env-default:"redis://localhost:6379/0"`
}

type Jobs struct {
	// FileIdleTTL: stored files not requested for this long are deleted
	// ("Time N"). Go duration syntax, e.g. 168h = 7 days.
	FileIdleTTL time.Duration `env:"FILE_IDLE_TTL" env-default:"168h"`
	// CleanupInterval: how often the idle-file cleanup runs.
	CleanupInterval time.Duration `env:"CLEANUP_INTERVAL" env-default:"1h"`
	// TaskResultTTL: how long a finished task's file can be downloaded by task ID.
	TaskResultTTL time.Duration `env:"TASK_RESULT_TTL" env-default:"1h"`
	TaskWorkers   int           `env:"TASK_WORKERS" env-default:"4"`
	// TaskLease: a worker that stops heartbeating for this long is presumed
	// dead and its task is handed to another worker.
	TaskLease          time.Duration `env:"TASK_LEASE" env-default:"2m"`
	WebhookTimeout     time.Duration `env:"WEBHOOK_TIMEOUT" env-default:"60s"`
	WebhookMaxAttempts int           `env:"WEBHOOK_MAX_ATTEMPTS" env-default:"6"`
	// WebhookAllowPrivate disables the SSRF guard that blocks webhook
	// deliveries to loopback/private/link-local addresses. Dev and tests only.
	WebhookAllowPrivate bool `env:"WEBHOOK_ALLOW_PRIVATE" env-default:"false"`
}

type Limits struct {
	// RateLimitPerMinute and RateLimitBurst apply per API key.
	RateLimitPerMinute int `env:"RATE_LIMIT_PER_MINUTE" env-default:"60"`
	RateLimitBurst     int `env:"RATE_LIMIT_BURST" env-default:"20"`
	// CacheTTL bounds how stale a cached read (lists, history) can be.
	CacheTTL time.Duration `env:"CACHE_TTL" env-default:"30s"`
}

// BootstrapAdmin creates the first administrator on startup when no user has
// Email yet. APIKey is optional: if empty, a key is generated and logged once.
type BootstrapAdmin struct {
	Username string `env:"BOOTSTRAP_ADMIN_USERNAME" env-default:"admin"`
	Email    string `env:"BOOTSTRAP_ADMIN_EMAIL"`
	APIKey   string `env:"BOOTSTRAP_ADMIN_API_KEY"`
}

// Storage selects and configures the backend finished files are saved to.
type Storage struct {
	// Backend: local | s3.
	Backend  string `env:"STORAGE_BACKEND" env-default:"local"`
	LocalDir string `env:"STORAGE_LOCAL_DIR" env-default:"./data"`

	// S3 settings. Credentials come from the standard AWS chain
	// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, shared config, IAM role).
	S3Bucket string `env:"S3_BUCKET"`
	S3Region string `env:"S3_REGION" env-default:"us-east-1"`
	// S3Endpoint overrides the AWS endpoint for S3-compatible stores (MinIO, R2).
	S3Endpoint string `env:"S3_ENDPOINT"`
	// S3PathStyle is required by most self-hosted S3-compatible stores.
	S3PathStyle bool   `env:"S3_PATH_STYLE" env-default:"false"`
	S3Prefix    string `env:"S3_PREFIX"`
	// S3CreateBucket creates S3Bucket on startup if it does not exist (for
	// self-hosted stores, e.g. the compose stack). Needs s3:CreateBucket.
	S3CreateBucket bool `env:"S3_CREATE_BUCKET" env-default:"false"`
}

func Load() (*Config, error) {
	var cfg Config
	var err error
	if _, statErr := os.Stat(".env"); statErr == nil {
		err = cleanenv.ReadConfig(".env", &cfg)
	} else {
		err = cleanenv.ReadEnv(&cfg)
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "ytdl-service")
	}
	return &cfg, err
}

// MaxDuration is MaxDurationSeconds as a Duration (0 = no limit).
func (c *Config) MaxDuration() time.Duration {
	return time.Duration(c.MaxDurationSeconds) * time.Second
}

// LogAttrs returns the effective configuration for the startup log line.
// The database password is never included; AWS credentials never live here
// (they come from the AWS chain).
func (c *Config) LogAttrs() []any {
	return []any{
		slog.String("database", databaseLabel(c.DatabaseURL)),
		slog.String("ytdlp_cookies", c.YtdlpCookies),
		slog.Bool("ytdlp_self_update", c.YtdlpSelfUpdate),
		slog.String("work_dir", c.WorkDir),
		slog.Int("max_duration_seconds", c.MaxDurationSeconds),
		slog.String("log_level", c.LogLevel),
		slog.String("log_format", c.LogFormat),
		slog.String("storage_backend", c.Storage.Backend),
		slog.String("storage_local_dir", c.Storage.LocalDir),
		slog.String("s3_bucket", c.Storage.S3Bucket),
		slog.String("s3_region", c.Storage.S3Region),
		slog.String("s3_endpoint", c.Storage.S3Endpoint),
		slog.Bool("s3_path_style", c.Storage.S3PathStyle),
		slog.String("s3_prefix", c.Storage.S3Prefix),
		slog.Bool("s3_create_bucket", c.Storage.S3CreateBucket),
		slog.String("http_addr", c.HTTP.Addr),
		slog.String("redis", redisLabel(c.HTTP.RedisURL)),
		slog.Duration("file_idle_ttl", c.Jobs.FileIdleTTL),
		slog.Duration("cleanup_interval", c.Jobs.CleanupInterval),
		slog.Duration("task_result_ttl", c.Jobs.TaskResultTTL),
		slog.Int("task_workers", c.Jobs.TaskWorkers),
		slog.Duration("task_lease", c.Jobs.TaskLease),
		slog.Duration("webhook_timeout", c.Jobs.WebhookTimeout),
		slog.Int("webhook_max_attempts", c.Jobs.WebhookMaxAttempts),
		slog.Bool("webhook_allow_private", c.Jobs.WebhookAllowPrivate),
		slog.Int("rate_limit_per_minute", c.Limits.RateLimitPerMinute),
		slog.Int("rate_limit_burst", c.Limits.RateLimitBurst),
		slog.Duration("cache_ttl", c.Limits.CacheTTL),
		slog.String("bootstrap_admin_email", c.Admin.Email),
		slog.Bool("bootstrap_admin_api_key_set", c.Admin.APIKey != ""),
	}
}

// databaseLabel renders a connection string as user@host:port/db for logs,
// dropping the password whichever DSN form was used.
func databaseLabel(dsn string) string {
	if dsn == "" {
		return "missing"
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "unparseable"
	}
	return fmt.Sprintf("%s@%s:%d/%s", cfg.User, cfg.Host, cfg.Port, cfg.Database)
}

// redisLabel renders a Redis URL without its password.
func redisLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "unparseable"
	}
	return u.Redacted()
}

// Validate rejects settings that would make the service misbehave quietly.
func (c *Config) Validate() error {
	switch {
	case c.Jobs.FileIdleTTL <= 0:
		return fmt.Errorf("FILE_IDLE_TTL must be positive")
	case c.Jobs.CleanupInterval <= 0:
		return fmt.Errorf("CLEANUP_INTERVAL must be positive")
	case c.Jobs.TaskResultTTL <= 0:
		return fmt.Errorf("TASK_RESULT_TTL must be positive")
	case c.Jobs.TaskWorkers < 1:
		return fmt.Errorf("TASK_WORKERS must be at least 1")
	case c.Jobs.TaskLease < 10*time.Second:
		return fmt.Errorf("TASK_LEASE must be at least 10s")
	case c.Jobs.WebhookMaxAttempts < 1:
		return fmt.Errorf("WEBHOOK_MAX_ATTEMPTS must be at least 1")
	case c.Limits.RateLimitPerMinute < 1:
		return fmt.Errorf("RATE_LIMIT_PER_MINUTE must be at least 1")
	case c.Limits.RateLimitBurst < 1:
		return fmt.Errorf("RATE_LIMIT_BURST must be at least 1")
	}
	return nil
}
