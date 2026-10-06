package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

// ErrInvalidAPIKey covers unknown, malformed and revoked keys alike, so a
// caller cannot tell which (and neither can an attacker probing keys).
var ErrInvalidAPIKey = errors.New("db: invalid or revoked API key")

const (
	apiKeyPrefix = "ytdl_"
	// apiKeyBytes of randomness: 256 bits, so the key cannot be guessed and
	// a plain SHA-256 is a sufficient at-rest hash (no bcrypt/argon2 needed).
	apiKeyBytes = 32
	// displayPrefixLen is how much of the key is kept in clear for listings
	// ("ytdl_" + 8 chars).
	displayPrefixLen = len(apiKeyPrefix) + 8
)

func hashAPIKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

func newAPIKey() (string, error) {
	b := make([]byte, apiKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate API key: %w", err)
	}
	return apiKeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateAPIKey issues a new key for the user with the given email. The
// plaintext is returned only here; the database keeps its hash.
func (d *DB) CreateAPIKey(ctx context.Context, email, name string) (string, dbgen.CreateAPIKeyRow, error) {
	user, err := d.GetUserByEmail(ctx, email)
	if err != nil {
		if IsNotFound(err) {
			return "", dbgen.CreateAPIKeyRow{}, fmt.Errorf("%w: user with email %q", ErrNotFound, email)
		}
		return "", dbgen.CreateAPIKeyRow{}, err
	}
	plaintext, err := newAPIKey()
	if err != nil {
		return "", dbgen.CreateAPIKeyRow{}, err
	}
	key, err := d.Queries.CreateAPIKey(ctx, dbgen.CreateAPIKeyParams{
		UserID:  user.ID,
		KeyHash: hashAPIKey(plaintext),
		Prefix:  plaintext[:displayPrefixLen],
		Name:    name,
	})
	if err != nil {
		return "", dbgen.CreateAPIKeyRow{}, err
	}
	d.log.Info("API key created",
		slog.Int64("user_id", user.ID), slog.Int64("key_id", key.ID), slog.String("prefix", key.Prefix))
	return plaintext, key, nil
}

// Authenticate resolves a plaintext key to its owner and records the use.
// Lookup is by hash, so there is no per-character comparison to time.
func (d *DB) Authenticate(ctx context.Context, plaintext string) (dbgen.User, error) {
	if !strings.HasPrefix(plaintext, apiKeyPrefix) {
		return dbgen.User{}, ErrInvalidAPIKey
	}
	row, err := d.GetUserByAPIKeyHash(ctx, hashAPIKey(plaintext))
	if err != nil {
		if IsNotFound(err) {
			return dbgen.User{}, ErrInvalidAPIKey
		}
		return dbgen.User{}, err
	}
	// A failed touch only loses the last-used timestamp; the key is valid.
	if err := d.TouchAPIKey(ctx, row.ApiKeyID); err != nil {
		d.log.Warn("recording API key use failed", slog.Int64("key_id", row.ApiKeyID), slog.Any("err", err))
	}
	return row.User, nil
}
