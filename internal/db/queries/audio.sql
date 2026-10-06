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
    last_uploaded_at = now(),
    last_requested_at = now()
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

-- name: TouchAudioRequested :execrows
-- Called whenever a stored file is served; keeps it from idle eviction.
UPDATE audio SET last_requested_at = now() WHERE source_id = $1 AND format = $2;

-- name: GetAudioByURL :one
SELECT * FROM audio WHERE url = $1 AND format = $2 ORDER BY last_requested_at DESC LIMIT 1;

-- name: ListIdleAudio :many
-- Stored files not requested since the cutoff and not pinned by a task whose
-- result is still downloadable.
SELECT * FROM audio a
WHERE a.storage_key IS NOT NULL
  AND a.last_requested_at < sqlc.arg(cutoff)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM tasks t
      WHERE t.storage_key = a.storage_key
        AND (t.expires_at IS NULL OR t.expires_at > now())
        AND t.status IN ('queued', 'running', 'succeeded'))
ORDER BY a.last_requested_at
LIMIT sqlc.arg(batch_size);

-- name: CountAudio :one
SELECT count(*) FROM audio;
