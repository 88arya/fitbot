-- name: InsertInbound :one
-- Returns no rows when the message was already stored, so callers can skip
-- retried webhooks.
INSERT INTO messages (user_id, direction, body, provider_id, status, parts, cost_usd)
VALUES ($1, 'inbound', $2, $3, 'received', $4, $5)
ON CONFLICT (provider_id) DO NOTHING
RETURNING *;

-- name: InsertOutbound :one
INSERT INTO messages (user_id, direction, body, status)
VALUES ($1, 'outbound', $2, 'sending')
RETURNING *;

-- name: MarkOutboundSent :exec
UPDATE messages SET provider_id = $2, status = 'queued', updated_at = now()
WHERE id = $1;

-- name: MarkOutboundFailed :exec
UPDATE messages SET status = 'failed', updated_at = now()
WHERE id = $1;

-- name: UpdateDelivery :execrows
UPDATE messages
SET status = $2,
    parts = COALESCE(sqlc.narg(parts), parts),
    cost_usd = COALESCE(sqlc.narg(cost_usd), cost_usd),
    updated_at = now()
WHERE provider_id = $1;

-- name: RecentMessages :many
-- The user's last messages, oldest first.
SELECT * FROM (
    SELECT * FROM messages WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2
) recent
ORDER BY created_at, id;

-- name: InsertLLMCall :exec
INSERT INTO llm_calls (message_id, model, input_tokens, output_tokens, latency_ms, cost_usd, error)
VALUES ($1, $2, $3, $4, $5, $6, $7);
