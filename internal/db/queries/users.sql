-- name: CreateUser :one
INSERT INTO users (username, email, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower(sqlc.arg(email));

-- name: ListUsers :many
SELECT * FROM users
ORDER BY id
LIMIT $1 OFFSET $2;

-- name: UpdateUserRole :one
UPDATE users SET role = $2 WHERE id = $1
RETURNING *;

-- name: DeleteUser :execrows
-- Cascades to the user's API keys.
DELETE FROM users WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: UpdateUser :one
-- Partial update: NULL arguments keep the current value.
UPDATE users
SET username = coalesce(sqlc.narg(username), username),
    email    = coalesce(sqlc.narg(email), email),
    role     = coalesce(sqlc.narg(role), role)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetWebhookSecret :one
UPDATE users SET webhook_secret = $2 WHERE id = $1
RETURNING webhook_secret;
