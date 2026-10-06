package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DanielTsarof/ytdl-service-ramona/internal/db/dbgen"
)

// Tests run against a real PostgreSQL server. Each test creates its own
// randomly named database and drops it afterwards, so runs never collide:
//
//	docker run -d --rm --name ytdl-pg-test -e POSTGRES_PASSWORD=test -p 15432:5432 postgres:17-alpine
//	TEST_DATABASE_URL=postgres://postgres:test@localhost:15432/postgres?sslmode=disable go test ./internal/db -v
//
// TEST_DATABASE_URL must point at a role allowed to CREATE DATABASE.

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newTestDB(t *testing.T) *DB {
	t.Helper()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	b := make([]byte, 6)
	rand.Read(b)
	name := "ytdl_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create database: %v", err)
	}

	cfg, err := pgconn.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	d, err := Open(ctx, connString(cfg), discard)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		admin.Close(ctx)
	})

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// connString rebuilds a URL from a parsed config (pgconn has no formatter),
// so the admin DSN can be either URL or keyword/value form.
func connString(c *pgconn.Config) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))),
		Path:   "/" + c.Database,
	}
	if c.TLSConfig == nil {
		u.RawQuery = "sslmode=disable"
	}
	return u.String()
}

func ptr[T any](v T) *T { return &v }

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

const uniqueViolation = "23505"

func TestMigrateUpDownUp(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Second Up is a no-op.
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	if err := d.MigrateDownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	var tables int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name IN ('users','api_keys','videos','audio')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("%d tables left after down migration", tables)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

func TestVideoUpsert(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	first, err := d.UpsertVideo(ctx, dbgen.UpsertVideoParams{
		SourceID: "abc", Url: "https://youtu.be/abc", Title: "Old", DurationSeconds: ptr[int32](19),
		StorageKey: ptr("media/abc/mp4.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := d.UpsertVideo(ctx, dbgen.UpsertVideoParams{
		SourceID: "abc", Url: "https://www.youtube.com/watch?v=abc", Title: "New",
		StorageKey: ptr("media/abc/mp4.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("upsert created a new row: %d != %d", second.ID, first.ID)
	}
	if !second.LastUploadedAt.After(first.LastUploadedAt) {
		t.Errorf("last_uploaded_at not refreshed: %v -> %v", first.LastUploadedAt, second.LastUploadedAt)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at changed on upsert")
	}
	if second.Title != "New" || second.Url != "https://www.youtube.com/watch?v=abc" || second.DurationSeconds != nil {
		t.Errorf("row not updated: %+v", second)
	}

	n, err := d.ClearVideoStorageKey(ctx, "abc")
	if err != nil || n != 1 {
		t.Fatalf("clear storage key: %d, %v", n, err)
	}
	got, err := d.GetVideoBySourceID(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.StorageKey != nil {
		t.Errorf("storage_key = %q, want NULL", *got.StorageKey)
	}

	list, err := d.ListVideos(ctx, dbgen.ListVideosParams{Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d rows, %v", len(list), err)
	}

	if n, err := d.DeleteVideo(ctx, "abc"); err != nil || n != 1 {
		t.Fatalf("delete: %d, %v", n, err)
	}
	if _, err := d.GetVideoBySourceID(ctx, "abc"); !IsNotFound(err) {
		t.Fatalf("get after delete: %v, want not found", err)
	}
}

func TestAudioFormatsAreSeparateRows(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	mp3, err := d.UpsertAudio(ctx, dbgen.UpsertAudioParams{
		SourceID: "abc", Format: dbgen.AudioFormatMp3, Url: "u", StorageKey: ptr("media/abc/mp3.mp3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	wav, err := d.UpsertAudio(ctx, dbgen.UpsertAudioParams{
		SourceID: "abc", Format: dbgen.AudioFormatWav, Url: "u", StorageKey: ptr("media/abc/wav.wav"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if mp3.ID == wav.ID {
		t.Fatal("mp3 and wav share a row")
	}

	again, err := d.UpsertAudio(ctx, dbgen.UpsertAudioParams{
		SourceID: "abc", Format: dbgen.AudioFormatMp3, Url: "u2", StorageKey: ptr("media/abc/mp3.mp3"),
	})
	if err != nil {
		t.Fatalf("repeat (source_id, format) should update: %v", err)
	}
	if again.ID != mp3.ID || again.Url != "u2" {
		t.Errorf("repeat upsert: %+v", again)
	}

	list, err := d.ListAudio(ctx, dbgen.ListAudioParams{Limit: 10})
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %d rows, %v", len(list), err)
	}

	if n, err := d.ClearAudioStorageKey(ctx, dbgen.ClearAudioStorageKeyParams{SourceID: "abc", Format: dbgen.AudioFormatWav}); err != nil || n != 1 {
		t.Fatalf("clear: %d, %v", n, err)
	}
	got, err := d.GetAudio(ctx, dbgen.GetAudioParams{SourceID: "abc", Format: dbgen.AudioFormatMp3})
	if err != nil || got.StorageKey == nil {
		t.Fatalf("clearing wav touched mp3: %+v, %v", got, err)
	}

	// Values outside the enum are rejected by Postgres.
	_, err = d.UpsertAudio(ctx, dbgen.UpsertAudioParams{SourceID: "abc", Format: "flac", Url: "u"})
	if err == nil {
		t.Error("format flac accepted")
	}
}

func TestUsers(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	u, err := d.CreateUser(ctx, dbgen.CreateUserParams{Username: "alice", Email: "Alice@Example.com", Role: dbgen.UserRoleUser})
	if err != nil {
		t.Fatal(err)
	}
	if u.RegisteredAt.IsZero() {
		t.Error("registered_at not set")
	}

	_, err = d.CreateUser(ctx, dbgen.CreateUserParams{Username: "alice", Email: "other@example.com", Role: dbgen.UserRoleUser})
	if pgCode(err) != uniqueViolation {
		t.Errorf("duplicate username: got %v, want unique violation", err)
	}
	_, err = d.CreateUser(ctx, dbgen.CreateUserParams{Username: "alice2", Email: "alice@EXAMPLE.com", Role: dbgen.UserRoleUser})
	if pgCode(err) != uniqueViolation {
		t.Errorf("duplicate email (case-insensitive): got %v, want unique violation", err)
	}

	byEmail, err := d.GetUserByEmail(ctx, "ALICE@example.COM")
	if err != nil || byEmail.ID != u.ID {
		t.Fatalf("lookup by email: %+v, %v", byEmail, err)
	}

	admin, err := d.UpdateUserRole(ctx, dbgen.UpdateUserRoleParams{ID: u.ID, Role: dbgen.UserRoleAdmin})
	if err != nil || admin.Role != dbgen.UserRoleAdmin {
		t.Fatalf("update role: %+v, %v", admin, err)
	}

	// The column default is 'user' when a raw insert omits the role.
	var role string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO users (username, email) VALUES ('bob', 'bob@example.com') RETURNING role`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "user" {
		t.Errorf("default role = %q", role)
	}

	users, err := d.ListUsers(ctx, dbgen.ListUsersParams{Limit: 10})
	if err != nil || len(users) != 2 {
		t.Fatalf("list users: %d, %v", len(users), err)
	}
}

func TestAPIKeys(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	alice, err := d.CreateUser(ctx, dbgen.CreateUserParams{Username: "alice", Email: "alice@example.com", Role: dbgen.UserRoleUser})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := d.CreateUser(ctx, dbgen.CreateUserParams{Username: "bob", Email: "bob@example.com", Role: dbgen.UserRoleUser})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := d.CreateAPIKey(ctx, "nobody@example.com", "x"); !IsNotFound(err) {
		t.Fatalf("key for unknown email: %v, want not found", err)
	}

	plain, key, err := d.CreateAPIKey(ctx, "ALICE@example.com", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, apiKeyPrefix) || !strings.HasPrefix(plain, key.Prefix) || key.UserID != alice.ID {
		t.Fatalf("key = %q, row = %+v", plain, key)
	}

	// The plaintext is never stored.
	var stored int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE key_hash = $1`, []byte(plain)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatal("plaintext key found in key_hash")
	}

	principal, err := d.Authenticate(ctx, plain)
	if err != nil || principal.User.ID != alice.ID || principal.KeyID != key.ID || principal.KeyPrefix != key.Prefix {
		t.Fatalf("authenticate: %+v, %v", principal, err)
	}
	keys, err := d.ListAPIKeysByUser(ctx, alice.ID)
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("last_used_at not recorded: %+v, %v", keys, err)
	}

	for _, bad := range []string{"", "nope", plain + "x", "ytdl_" + strings.Repeat("A", 43)} {
		if _, err := d.Authenticate(ctx, bad); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("Authenticate(%q): %v, want ErrInvalidAPIKey", bad, err)
		}
	}

	// Bob cannot revoke Alice's key.
	if n, err := d.RevokeAPIKey(ctx, dbgen.RevokeAPIKeyParams{ID: key.ID, UserID: bob.ID}); err != nil || n != 0 {
		t.Fatalf("cross-user revoke: %d, %v", n, err)
	}
	if _, err := d.Authenticate(ctx, plain); err != nil {
		t.Fatalf("key stopped working after foreign revoke attempt: %v", err)
	}

	if n, err := d.RevokeAPIKey(ctx, dbgen.RevokeAPIKeyParams{ID: key.ID, UserID: alice.ID}); err != nil || n != 1 {
		t.Fatalf("revoke: %d, %v", n, err)
	}
	if _, err := d.Authenticate(ctx, plain); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("revoked key: %v, want ErrInvalidAPIKey", err)
	}

	// Deleting a user removes their keys.
	plain2, _, err := d.CreateAPIKey(ctx, "alice@example.com", "second")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := d.DeleteUser(ctx, alice.ID); err != nil || n != 1 {
		t.Fatalf("delete user: %d, %v", n, err)
	}
	if _, err := d.Authenticate(ctx, plain2); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("key of deleted user: %v, want ErrInvalidAPIKey", err)
	}
	var left int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id = $1`, alice.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("keys left after user delete: %d, %v", left, err)
	}
}

func TestInTxRollsBack(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	boom := errors.New("boom")
	err := d.InTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.CreateUser(ctx, dbgen.CreateUserParams{Username: "tx", Email: "tx@example.com", Role: dbgen.UserRoleUser}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx: %v", err)
	}
	if _, err := d.GetUserByEmail(ctx, "tx@example.com"); !IsNotFound(err) {
		t.Fatalf("user survived rollback: %v", err)
	}

	if err := d.InTx(ctx, func(q *dbgen.Queries) error {
		_, err := q.CreateUser(ctx, dbgen.CreateUserParams{Username: "tx", Email: "tx@example.com", Role: dbgen.UserRoleUser})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetUserByEmail(ctx, "tx@example.com"); err != nil {
		t.Fatalf("committed user missing: %v", err)
	}
}
