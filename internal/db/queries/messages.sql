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
-- The user's last messages up to the one with id through, oldest first, so
-- a reply never sees a text sent after the one it answers.
SELECT * FROM (
    SELECT * FROM messages WHERE user_id = $1 AND id <= sqlc.arg(through) ORDER BY created_at DESC, id DESC LIMIT $2
) recent
ORDER BY created_at, id;

-- name: MarkInbound :exec
-- Sets an inbound text's status: answered once its reply is sent, or
-- missed.
UPDATE messages SET status = $2, updated_at = now()
WHERE id = $1 AND direction = 'inbound';

-- name: UnansweredTexts :many
-- Inbound texts from the given phones still waiting for a reply, oldest
-- first.
SELECT m.* FROM messages m JOIN users u ON u.id = m.user_id
WHERE m.direction = 'inbound' AND m.status = 'received' AND u.phone = ANY(sqlc.arg(phones)::text[])
ORDER BY m.id;

-- name: InsertLLMCall :exec
INSERT INTO llm_calls (message_id, model, input_tokens, output_tokens, latency_ms, cost_usd, error)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CountMessages :one
SELECT count(*) FROM messages WHERE user_id = $1;
