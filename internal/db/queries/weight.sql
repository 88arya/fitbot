-- name: InsertWeight :one
INSERT INTO weight_logs (user_id, action_id, message_id, local_date, grams, logged_at)
VALUES (@user_id, @action_id, @message_id, @local_date, @grams, @logged_at)
RETURNING *;

-- name: GetWeight :one
SELECT * FROM weight_logs
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL
FOR UPDATE;

-- name: LastWeight :one
-- The user's most recently logged weigh-in that is not deleted.
SELECT * FROM weight_logs
WHERE user_id = @user_id AND deleted_at IS NULL
ORDER BY id DESC
LIMIT 1
FOR UPDATE;

-- name: UpdateWeight :execrows
UPDATE weight_logs SET local_date = @local_date, grams = @grams, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetWeightDeleted :execrows
-- A NULL deleted_at restores the row.
UPDATE weight_logs SET deleted_at = sqlc.narg(deleted_at), updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: WeightBetween :many
-- Weigh-ins on local days from first to last, oldest first, and within a
-- day in the order they were logged, so each day's first is its weight.
SELECT * FROM weight_logs
WHERE user_id = @user_id AND local_date BETWEEN @first AND @last AND deleted_at IS NULL
ORDER BY local_date, logged_at, id;

-- name: WeightThrough :one
-- The weight of the latest day weighed on or before day: that day's first
-- weigh-in.
SELECT * FROM weight_logs
WHERE user_id = @user_id AND local_date <= @day AND deleted_at IS NULL
ORDER BY local_date DESC, logged_at, id
LIMIT 1;
