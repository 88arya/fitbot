-- name: PausesBetween :many
-- The user's pauses that cover any day from first to last, oldest first.
SELECT * FROM pauses
WHERE user_id = @user_id AND deleted_at IS NULL AND first_date <= @last AND last_date >= @first
ORDER BY first_date, id;

-- name: InsertPause :one
INSERT INTO pauses (user_id, first_date, last_date, reason)
VALUES (@user_id, @first_date, @last_date, sqlc.narg(reason))
RETURNING *;

-- name: SetPauseDeleted :execrows
-- A NULL deleted_at restores the row.
UPDATE pauses SET deleted_at = sqlc.narg(deleted_at), updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetPauseLast :execrows
UPDATE pauses SET last_date = @last_date, updated_at = now()
WHERE id = @id AND user_id = @user_id;
