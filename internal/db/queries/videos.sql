-- name: UpsertVideo :one
-- Records an upload to storage; a repeat upload of the same video refreshes
-- the row and its last_uploaded_at.
INSERT INTO videos (source_id, quality, url, title, duration_seconds, storage_key)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (source_id, quality) DO UPDATE
SET url              = EXCLUDED.url,
    title            = EXCLUDED.title,
    duration_seconds = EXCLUDED.duration_seconds,
    storage_key      = EXCLUDED.storage_key,
    last_uploaded_at = now(),
    last_requested_at = now()
RETURNING *;

-- name: GetVideo :one
SELECT * FROM videos WHERE source_id = $1 AND quality = $2;

-- name: ListVideosBySourceID :many
-- Every stored quality of one video.
SELECT * FROM videos WHERE source_id = $1 ORDER BY quality;

-- name: ListVideos :many
SELECT * FROM videos
ORDER BY last_uploaded_at DESC, id DESC
LIMIT $1 OFFSET $2;

-- name: ClearVideoStorageKey :execrows
-- Marks the file as no longer stored (e.g. evicted); the row is kept.
UPDATE videos SET storage_key = NULL WHERE source_id = $1 AND quality = $2;

-- name: DeleteVideo :execrows
DELETE FROM videos WHERE source_id = $1 AND quality = $2;

-- name: DeleteVideosBySourceID :execrows
-- Removes every quality of one video.
DELETE FROM videos WHERE source_id = $1;

-- name: TouchVideoRequested :execrows
-- Called whenever a stored file is served; keeps it from idle eviction.
UPDATE videos SET last_requested_at = now() WHERE source_id = $1 AND quality = $2;

-- name: GetVideoByURL :one
SELECT * FROM videos WHERE url = $1 ORDER BY last_requested_at DESC LIMIT 1;

-- name: ListIdleVideos :many
-- Stored files not requested since the cutoff and not pinned by a task whose
-- result is still downloadable.
SELECT * FROM videos v
WHERE v.storage_key IS NOT NULL
  AND v.last_requested_at < sqlc.arg(cutoff)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM tasks t
      WHERE t.storage_key = v.storage_key
        AND (t.expires_at IS NULL OR t.expires_at > now())
        AND t.status IN ('queued', 'running', 'succeeded'))
ORDER BY v.last_requested_at
LIMIT sqlc.arg(batch_size);

-- name: CountVideos :one
SELECT count(*) FROM videos;
