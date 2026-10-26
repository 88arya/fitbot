-- name: SetGoal :exec
INSERT INTO goals (user_id, metric, value, start_date)
VALUES (@user_id, @metric, sqlc.narg(value), @start_date)
ON CONFLICT (user_id, metric, start_date) DO UPDATE
SET value = EXCLUDED.value, updated_at = now();

-- name: LatestGoals :many
-- Each metric's most recent goal, including ones the user stopped tracking.
-- A metric with no row was never set.
SELECT DISTINCT ON (metric) * FROM goals
WHERE user_id = @user_id
ORDER BY metric, start_date DESC;

-- name: GoalsOn :many
-- The goals in force on a local day.
SELECT DISTINCT ON (metric) * FROM goals
WHERE user_id = @user_id AND start_date <= @day
ORDER BY metric, start_date DESC;

-- name: GoalOnDay :one
-- The goal row that starts on day, if a change was made that day.
SELECT * FROM goals WHERE user_id = @user_id AND metric = @metric AND start_date = @start_date;

-- name: DeleteGoal :exec
DELETE FROM goals WHERE user_id = @user_id AND metric = @metric AND start_date = @start_date;
