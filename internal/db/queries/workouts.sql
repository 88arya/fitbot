-- name: InsertWorkout :one
INSERT INTO workouts (user_id, action_id, message_id, local_date, status, sessions, logged_at)
VALUES (@user_id, @action_id, @message_id, @local_date, @status, @sessions, @logged_at)
RETURNING *;

-- name: GetWorkout :one
SELECT * FROM workouts
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL
FOR UPDATE;

-- name: LastWorkout :one
-- The user's latest workout that is not deleted, by the day it happened.
SELECT * FROM workouts
WHERE user_id = @user_id AND deleted_at IS NULL
ORDER BY local_date DESC, id DESC
LIMIT 1
FOR UPDATE;

-- name: UpdateWorkout :execrows
UPDATE workouts
SET local_date = @local_date, status = @status, sessions = @sessions, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetWorkoutDeleted :execrows
-- A NULL deleted_at restores the row.
UPDATE workouts SET deleted_at = sqlc.narg(deleted_at), updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: WorkoutsBetween :many
-- Workouts on local days from first to last, oldest first.
SELECT * FROM workouts
WHERE user_id = @user_id AND local_date BETWEEN @first AND @last AND deleted_at IS NULL
ORDER BY local_date, id;
