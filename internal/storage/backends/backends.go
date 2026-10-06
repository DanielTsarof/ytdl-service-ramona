// Package backends builds the configured storage.Storage. It lives apart from
// package storage because the backends import storage for its types.
package backends

import (
	"context"
	"fmt"
	"strings"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/config"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/local"
	"github.com/DanielTsarof/ytdl-service-ramona/internal/storage/s3"
)

func New(ctx context.Context, cfg config.Storage) (storage.Storage, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Backend)) {
	case "", "local":
		return local.New(cfg.LocalDir)
	case "s3":
		return s3.New(ctx, s3.Options{
			Bucket:    cfg.S3Bucket,
			Region:    cfg.S3Region,
			Endpoint:  cfg.S3Endpoint,
			PathStyle: cfg.S3PathStyle,
			Prefix:    cfg.S3Prefix,

			CreateBucket: cfg.S3CreateBucket,
		})
	}
	return nil, fmt.Errorf("unknown STORAGE_BACKEND %q (want local or s3)", cfg.Backend)
}
