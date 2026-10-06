-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, key_hash, prefix, name)
VALUES ($1, $2, $3, $4)
RETURNING id, user_id, prefix, name, created_at, last_used_at, revoked_at;

-- name: GetUserByAPIKeyHash :one
-- Resolves an active (not revoked) key to its owner.
SELECT sqlc.embed(users), api_keys.id AS api_key_id
FROM api_keys
JOIN users ON users.id = api_keys.user_id
WHERE api_keys.key_hash = $1 AND api_keys.revoked_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1;

-- name: ListAPIKeysByUser :many
-- Never returns key_hash.
SELECT id, user_id, prefix, name, created_at, last_used_at, revoked_at
FROM api_keys
WHERE user_id = $1
ORDER BY id;

-- name: RevokeAPIKey :execrows
-- Scoped by user_id so one user cannot revoke another user's key.
UPDATE api_keys SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;
