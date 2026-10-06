-- name: UpsertVideo :one
-- Records an upload to storage; a repeat upload of the same video refreshes
-- the row and its last_uploaded_at.
INSERT INTO videos (source_id, url, title, duration_seconds, storage_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (source_id) DO UPDATE
SET url              = EXCLUDED.url,
    title            = EXCLUDED.title,
    duration_seconds = EXCLUDED.duration_seconds,
    storage_key      = EXCLUDED.storage_key,
    last_uploaded_at = now()
RETURNING *;

-- name: GetVideoBySourceID :one
SELECT * FROM videos WHERE source_id = $1;

-- name: ListVideos :many
SELECT * FROM videos
ORDER BY last_uploaded_at DESC, id DESC
LIMIT $1 OFFSET $2;

-- name: ClearVideoStorageKey :execrows
-- Marks the file as no longer stored (e.g. evicted); the row is kept.
UPDATE videos SET storage_key = NULL WHERE source_id = $1;

-- name: DeleteVideo :execrows
DELETE FROM videos WHERE source_id = $1;
