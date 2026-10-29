-- name: InsertIntake :one
INSERT INTO intake_logs (user_id, action_id, message_id, local_date, metric, amount, logged_at)
VALUES (@user_id, @action_id, @message_id, @local_date, @metric, @amount, @logged_at)
RETURNING *;

-- name: GetIntake :one
SELECT * FROM intake_logs
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL
FOR UPDATE;

-- name: LastIntake :one
-- The user's most recently logged amount of metric that is not deleted.
SELECT * FROM intake_logs
WHERE user_id = @user_id AND metric = @metric AND deleted_at IS NULL
ORDER BY id DESC
LIMIT 1
FOR UPDATE;

-- name: UpdateIntake :execrows
UPDATE intake_logs SET local_date = @local_date, amount = @amount, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetIntakeDeleted :execrows
-- A NULL deleted_at restores the row.
UPDATE intake_logs SET deleted_at = sqlc.narg(deleted_at), updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: IntakeOn :many
SELECT * FROM intake_logs
WHERE user_id = @user_id AND local_date = @day AND deleted_at IS NULL
ORDER BY logged_at, id;

-- name: IntakeTotals :many
-- A local day's total for each metric with anything logged.
SELECT metric, SUM(amount)::int AS total
FROM intake_logs
WHERE user_id = @user_id AND local_date = @day AND deleted_at IS NULL
GROUP BY metric;

-- name: IntakeDaily :many
-- Each local day's total of one metric from first to last, for the days
-- with anything logged, oldest first.
SELECT local_date, SUM(amount)::int AS total
FROM intake_logs
WHERE user_id = @user_id AND metric = @metric AND local_date BETWEEN @first AND @last AND deleted_at IS NULL
GROUP BY local_date
ORDER BY local_date;
