-- name: ListMemories :many
SELECT * FROM memories WHERE user_id = $1 ORDER BY id;

-- name: CountMemories :one
SELECT count(*) FROM memories WHERE user_id = $1;

-- name: InsertMemory :one
INSERT INTO memories (user_id, fact) VALUES ($1, $2)
RETURNING *;

-- name: UpdateMemory :execrows
UPDATE memories SET fact = $3, updated_at = now()
WHERE id = $1 AND user_id = $2;

-- name: DeleteMemory :execrows
DELETE FROM memories WHERE id = $1 AND user_id = $2;
