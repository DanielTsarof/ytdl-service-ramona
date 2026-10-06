-- name: CreateTask :one
-- Returns no row when (user_id, idempotency_key) already exists; the caller
-- then loads the existing task with GetTaskByIdempotency.
INSERT INTO tasks (user_id, api_key_id, idempotency_key, request_hash, query, source_url, format, quality, webhook_url)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (user_id, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1;

-- name: GetTaskByIdempotency :one
SELECT * FROM tasks WHERE user_id = $1 AND idempotency_key = $2;

-- name: FindReusableTask :one
-- Implicit idempotency: the newest identical request that is still in flight
-- or whose result is still downloadable.
SELECT * FROM tasks
WHERE user_id = $1 AND request_hash = $2
  AND (status IN ('queued', 'running') OR (status = 'succeeded' AND expires_at > now()))
ORDER BY created_at DESC
LIMIT 1;

-- name: SetTaskHistory :exec
UPDATE tasks SET history_id = $2 WHERE id = $1;

-- name: ClaimTask :one
-- Takes the oldest queued task, or a running one whose worker lease expired
-- (the worker died). SKIP LOCKED lets any number of workers and instances
-- poll concurrently without handing out the same task twice.
UPDATE tasks
SET status       = 'running',
    started_at   = coalesce(started_at, now()),
    locked_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8),
    attempts     = attempts + 1
WHERE id = (
    SELECT id FROM tasks
    WHERE (status = 'queued' OR (status = 'running' AND locked_until < now()))
      AND attempts < sqlc.arg(max_attempts)::integer
    ORDER BY created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1)
RETURNING *;

-- name: FailAbandonedTasks :execrows
-- Tasks whose worker died on the last allowed attempt would otherwise stay
-- "running" forever.
UPDATE tasks
SET status        = 'failed',
    error         = 'worker lost the task too many times',
    completed_at  = now(),
    locked_until  = NULL,
    webhook_state = CASE WHEN webhook_url IS NOT NULL THEN 'pending'::webhook_state ELSE 'none'::webhook_state END,
    next_webhook_at = now()
WHERE status = 'running' AND locked_until < now() AND attempts >= sqlc.arg(max_attempts)::integer;

-- name: ExtendTaskLease :execrows
UPDATE tasks SET locked_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8)
WHERE id = $1 AND status = 'running';

-- name: CompleteTask :one
UPDATE tasks
SET status          = 'succeeded',
    source_id       = $2,
    title           = $3,
    storage_key     = $4,
    error           = NULL,
    completed_at    = now(),
    expires_at      = now() + make_interval(secs => sqlc.arg(result_ttl_seconds)::float8),
    locked_until    = NULL,
    webhook_state   = CASE WHEN webhook_url IS NOT NULL THEN 'pending'::webhook_state ELSE 'none'::webhook_state END,
    next_webhook_at = now()
WHERE id = $1
RETURNING *;

-- name: FailTask :one
UPDATE tasks
SET status          = 'failed',
    error           = $2,
    completed_at    = now(),
    locked_until    = NULL,
    webhook_state   = CASE WHEN webhook_url IS NOT NULL THEN 'pending'::webhook_state ELSE 'none'::webhook_state END,
    next_webhook_at = now()
WHERE id = $1
RETURNING *;

-- name: ClaimWebhook :one
-- Pushes next_webhook_at forward as a delivery lease, so another poller does
-- not pick the same webhook while this delivery is in progress.
UPDATE tasks
SET next_webhook_at  = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8),
    webhook_attempts = webhook_attempts + 1
WHERE id = (
    SELECT id FROM tasks
    WHERE webhook_state = 'pending' AND next_webhook_at <= now()
    ORDER BY next_webhook_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1)
RETURNING *;

-- name: MarkWebhookDelivered :exec
UPDATE tasks SET webhook_state = 'delivered', webhook_error = NULL, next_webhook_at = NULL WHERE id = $1;

-- name: MarkWebhookRetry :exec
UPDATE tasks
SET webhook_error   = $2,
    next_webhook_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::float8)
WHERE id = $1;

-- name: MarkWebhookFailed :exec
UPDATE tasks SET webhook_state = 'failed', webhook_error = $2, next_webhook_at = NULL WHERE id = $1;
