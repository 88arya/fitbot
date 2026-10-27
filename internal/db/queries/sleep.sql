-- name: InsertSleep :one
INSERT INTO sleep_logs (user_id, action_id, message_id, local_date, minutes, bedtime, wake_time, logged_at)
VALUES (@user_id, @action_id, @message_id, @local_date, @minutes, sqlc.narg(bedtime), sqlc.narg(wake_time), @logged_at)
RETURNING *;

-- name: GetSleep :one
SELECT * FROM sleep_logs
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL
FOR UPDATE;

-- name: LastSleep :one
-- The user's latest night that is not deleted, by the day they woke up.
SELECT * FROM sleep_logs
WHERE user_id = @user_id AND deleted_at IS NULL
ORDER BY local_date DESC
LIMIT 1
FOR UPDATE;

-- name: SleepOn :one
-- The night that counts toward a local day, if one is logged.
SELECT * FROM sleep_logs
WHERE user_id = @user_id AND local_date = @day AND deleted_at IS NULL;

-- name: UpdateSleep :execrows
UPDATE sleep_logs
SET local_date = @local_date, minutes = @minutes, bedtime = sqlc.narg(bedtime), wake_time = sqlc.narg(wake_time),
    updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetSleepDeleted :execrows
-- A NULL deleted_at restores the row.
UPDATE sleep_logs SET deleted_at = sqlc.narg(deleted_at), updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SleepBetween :many
-- Nights counting toward local days from first to last, oldest first.
SELECT * FROM sleep_logs
WHERE user_id = @user_id AND local_date BETWEEN @first AND @last AND deleted_at IS NULL
ORDER BY local_date;
