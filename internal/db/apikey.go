package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	// MinAPIKeyLen is the shortest externally supplied key accepted (see
	// IssueAPIKeyWithPlaintext); generated keys are 48 characters.
	MinAPIKeyLen = 40
)

// Principal is an authenticated caller.
type Principal struct {
	User      dbgen.User
	KeyID     int64
	KeyPrefix string
}

func (p Principal) IsAdmin() bool { return p.User.Role == dbgen.UserRoleAdmin }

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

// NewWebhookSecret returns a fresh random webhook signing secret.
func NewWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate webhook secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// IssueAPIKey creates a new key for userID using q (pass d.Queries, or a
// transaction's queries). The plaintext is returned only here; the database
// keeps its hash.
func IssueAPIKey(ctx context.Context, q *dbgen.Queries, userID int64, name string) (string, dbgen.CreateAPIKeyRow, error) {
	plaintext, err := newAPIKey()
	if err != nil {
		return "", dbgen.CreateAPIKeyRow{}, err
	}
	key, err := IssueAPIKeyWithPlaintext(ctx, q, userID, name, plaintext)
	return plaintext, key, err
}

// IssueAPIKeyWithPlaintext registers a caller-chosen key, e.g. a bootstrap
// admin key from the environment. It must look like a generated key.
func IssueAPIKeyWithPlaintext(ctx context.Context, q *dbgen.Queries, userID int64, name, plaintext string) (dbgen.CreateAPIKeyRow, error) {
	if !strings.HasPrefix(plaintext, apiKeyPrefix) || len(plaintext) < MinAPIKeyLen {
		return dbgen.CreateAPIKeyRow{}, fmt.Errorf("API key must start with %q and be at least %d characters", apiKeyPrefix, MinAPIKeyLen)
	}
	return q.CreateAPIKey(ctx, dbgen.CreateAPIKeyParams{
		UserID:  userID,
		KeyHash: hashAPIKey(plaintext),
		Prefix:  plaintext[:displayPrefixLen],
		Name:    name,
	})
}

// CreateAPIKey issues a new key for the user with the given email.
func (d *DB) CreateAPIKey(ctx context.Context, email, name string) (string, dbgen.CreateAPIKeyRow, error) {
	user, err := d.GetUserByEmail(ctx, email)
	if err != nil {
		if IsNotFound(err) {
			return "", dbgen.CreateAPIKeyRow{}, fmt.Errorf("%w: user with email %q", ErrNotFound, email)
		}
		return "", dbgen.CreateAPIKeyRow{}, err
	}
	plaintext, key, err := IssueAPIKey(ctx, d.Queries, user.ID, name)
	if err != nil {
		return "", dbgen.CreateAPIKeyRow{}, err
	}
	d.log.Info("API key created",
		slog.Int64("user_id", user.ID), slog.Int64("key_id", key.ID), slog.String("prefix", key.Prefix))
	return plaintext, key, nil
}

// RefreshAPIKey revokes keyID and issues a replacement with the same name in
// one transaction, so the user is never left with neither key.
func (d *DB) RefreshAPIKey(ctx context.Context, userID, keyID int64) (string, dbgen.CreateAPIKeyRow, error) {
	var (
		plaintext string
		key       dbgen.CreateAPIKeyRow
	)
	err := d.InTx(ctx, func(q *dbgen.Queries) error {
		old, err := q.GetAPIKeyForUser(ctx, dbgen.GetAPIKeyForUserParams{ID: keyID, UserID: userID})
		if err != nil {
			return err
		}
		if old.RevokedAt != nil {
			return fmt.Errorf("%w: key %d is already revoked", ErrNotFound, keyID)
		}
		if _, err := q.RevokeAPIKey(ctx, dbgen.RevokeAPIKeyParams{ID: keyID, UserID: userID}); err != nil {
			return err
		}
		plaintext, key, err = IssueAPIKey(ctx, q, userID, old.Name)
		return err
	})
	if err != nil {
		return "", dbgen.CreateAPIKeyRow{}, err
	}
	d.log.Info("API key refreshed",
		slog.Int64("user_id", userID), slog.Int64("old_key_id", keyID), slog.Int64("new_key_id", key.ID))
	return plaintext, key, nil
}

// Authenticate resolves a plaintext key to its owner and records the use.
// Lookup is by hash, so there is no per-character comparison to time.
func (d *DB) Authenticate(ctx context.Context, plaintext string) (Principal, error) {
	if !strings.HasPrefix(plaintext, apiKeyPrefix) {
		return Principal{}, ErrInvalidAPIKey
	}
	row, err := d.GetUserByAPIKeyHash(ctx, hashAPIKey(plaintext))
	if err != nil {
		if IsNotFound(err) {
			return Principal{}, ErrInvalidAPIKey
		}
		return Principal{}, err
	}
	// A failed touch only loses the last-used timestamp; the key is valid.
	if err := d.TouchAPIKey(ctx, row.ApiKeyID); err != nil {
		d.log.Warn("recording API key use failed", slog.Int64("key_id", row.ApiKeyID), slog.Any("err", err))
	}
	return Principal{User: row.User, KeyID: row.ApiKeyID, KeyPrefix: row.ApiKeyPrefix}, nil
}

// BootstrapAdmin makes sure an admin with email exists. When it creates one,
// it registers apiKey (if non-empty) or generates a key and returns it, so
// the caller can show it once. created is false when the user already exists.
func (d *DB) BootstrapAdmin(ctx context.Context, username, email, apiKey string) (generatedKey string, created bool, err error) {
	if _, err := d.GetUserByEmail(ctx, email); err == nil {
		return "", false, nil
	} else if !IsNotFound(err) {
		return "", false, err
	}
	err = d.InTx(ctx, func(q *dbgen.Queries) error {
		u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Username: username, Email: email, Role: dbgen.UserRoleAdmin})
		if err != nil {
			return err
		}
		if apiKey != "" {
			_, err = IssueAPIKeyWithPlaintext(ctx, q, u.ID, "bootstrap", apiKey)
			return err
		}
		generatedKey, _, err = IssueAPIKey(ctx, q, u.ID, "bootstrap")
		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("bootstrap admin: %w", err)
	}
	return generatedKey, true, nil
}
