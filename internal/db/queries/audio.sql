-- name: UpsertAudio :one
-- Records an upload to storage; a repeat upload of the same video in the same
-- format refreshes the row and its last_uploaded_at.
INSERT INTO audio (source_id, format, url, title, duration_seconds, storage_key)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (source_id, format) DO UPDATE
SET url              = EXCLUDED.url,
    title            = EXCLUDED.title,
    duration_seconds = EXCLUDED.duration_seconds,
    storage_key      = EXCLUDED.storage_key,
    last_uploaded_at = now()
RETURNING *;

-- name: GetAudio :one
SELECT * FROM audio WHERE source_id = $1 AND format = $2;

-- name: ListAudio :many
SELECT * FROM audio
ORDER BY last_uploaded_at DESC, id DESC
LIMIT $1 OFFSET $2;

-- name: ClearAudioStorageKey :execrows
-- Marks the file as no longer stored (e.g. evicted); the row is kept.
UPDATE audio SET storage_key = NULL WHERE source_id = $1 AND format = $2;

-- name: DeleteAudio :execrows
DELETE FROM audio WHERE source_id = $1 AND format = $2;
