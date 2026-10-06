package config

import (
	"fmt"
	"log/slog"
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
