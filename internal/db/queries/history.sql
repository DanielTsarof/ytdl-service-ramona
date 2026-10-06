-- name: InsertHistory :one
INSERT INTO request_history (user_id, api_key_id, kind, query, source_url, format, task_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id;

-- name: FinishHistory :exec
UPDATE request_history
SET status      = sqlc.arg(status),
    http_status = sqlc.narg(http_status),
    source_id   = coalesce(sqlc.narg(source_id), source_id),
    title       = coalesce(nullif(sqlc.arg(title)::text, ''), title),
    from_cache  = sqlc.arg(from_cache),
    error       = sqlc.narg(error),
    duration_ms = (extract(epoch FROM now() - created_at) * 1000)::bigint
WHERE id = sqlc.arg(id);

-- name: ListHistory :many
-- Every filter is optional (NULL = no filter). search must already have its
-- LIKE wildcards escaped by the caller.
SELECT * FROM request_history
WHERE (sqlc.narg(user_id)::bigint IS NULL OR user_id = sqlc.narg(user_id))
  AND (sqlc.narg(kind)::request_kind IS NULL OR kind = sqlc.narg(kind))
  AND (sqlc.narg(status)::request_status IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(format)::media_format IS NULL OR format = sqlc.narg(format))
  AND (sqlc.narg(created_from)::timestamptz IS NULL OR created_at >= sqlc.narg(created_from))
  AND (sqlc.narg(created_to)::timestamptz IS NULL OR created_at < sqlc.narg(created_to))
  AND (sqlc.narg(search)::text IS NULL
       OR query ILIKE '%' || sqlc.narg(search) || '%'
       OR source_url ILIKE '%' || sqlc.narg(search) || '%'
       OR title ILIKE '%' || sqlc.narg(search) || '%')
ORDER BY
  CASE WHEN sqlc.arg(sort_asc)::boolean THEN created_at END ASC,
  CASE WHEN sqlc.arg(sort_asc)::boolean THEN id END ASC,
  CASE WHEN NOT sqlc.arg(sort_asc)::boolean THEN created_at END DESC,
  CASE WHEN NOT sqlc.arg(sort_asc)::boolean THEN id END DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountHistory :one
SELECT count(*) FROM request_history
WHERE (sqlc.narg(user_id)::bigint IS NULL OR user_id = sqlc.narg(user_id))
  AND (sqlc.narg(kind)::request_kind IS NULL OR kind = sqlc.narg(kind))
  AND (sqlc.narg(status)::request_status IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(format)::media_format IS NULL OR format = sqlc.narg(format))
  AND (sqlc.narg(created_from)::timestamptz IS NULL OR created_at >= sqlc.narg(created_from))
  AND (sqlc.narg(created_to)::timestamptz IS NULL OR created_at < sqlc.narg(created_to))
  AND (sqlc.narg(search)::text IS NULL
       OR query ILIKE '%' || sqlc.narg(search) || '%'
       OR source_url ILIKE '%' || sqlc.narg(search) || '%'
       OR title ILIKE '%' || sqlc.narg(search) || '%');
