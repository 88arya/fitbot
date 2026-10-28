-- name: SupplementByName :one
SELECT * FROM supplements WHERE user_id = @user_id AND name = @name;

-- name: InsertSupplement :one
INSERT INTO supplements (user_id, name) VALUES (@user_id, @name)
RETURNING *;

-- name: SupplementHistory :many
-- Every plan row of the user's supplements, by name and oldest first, so a
-- range of days can each use the plan in force then.
SELECT p.supplement_id, s.name, p.start_date, p.active, p.dose, p.take_at, p.anchor, p.days
FROM supplement_plans p JOIN supplements s ON s.id = p.supplement_id
WHERE s.user_id = @user_id
ORDER BY s.name, p.start_date;

-- name: SupplementPlanOnDay :one
-- The plan row that starts on start_date, if a change was made that day.
SELECT * FROM supplement_plans WHERE supplement_id = @supplement_id AND start_date = @start_date;

-- name: SetSupplementPlan :exec
INSERT INTO supplement_plans (supplement_id, start_date, active, dose, take_at, anchor, days)
VALUES (@supplement_id, @start_date, @active, sqlc.narg(dose), sqlc.narg(take_at), sqlc.narg(anchor), sqlc.narg(days))
ON CONFLICT (supplement_id, start_date) DO UPDATE
SET active = EXCLUDED.active, dose = EXCLUDED.dose, take_at = EXCLUDED.take_at, anchor = EXCLUDED.anchor,
    days = EXCLUDED.days;

-- name: DeleteSupplementPlan :exec
DELETE FROM supplement_plans WHERE supplement_id = @supplement_id AND start_date = @start_date;

-- name: SetSupplementsAnswered :exec
UPDATE users SET supplements_answered = @answered WHERE id = @id;

-- name: InsertSupplementLog :one
INSERT INTO supplement_logs (user_id, action_id, message_id, supplement_id, local_date, status, logged_at)
VALUES (@user_id, @action_id, @message_id, @supplement_id, @local_date, @status, @logged_at)
RETURNING *;

-- name: GetSupplementLog :one
SELECT * FROM supplement_logs
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL
FOR UPDATE;

-- name: LastSupplementLog :one
-- The user's latest supplement log that is not deleted, by the day it's for.
SELECT * FROM supplement_logs
WHERE user_id = @user_id AND deleted_at IS NULL
ORDER BY local_date DESC, id DESC
LIMIT 1
FOR UPDATE;

-- name: UpdateSupplementLog :execrows
UPDATE supplement_logs
SET local_date = @local_date, status = @status, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetSupplementLogDeleted :execrows
-- A NULL deleted_at restores the row.
UPDATE supplement_logs SET deleted_at = sqlc.narg(deleted_at), updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SupplementLogsBetween :many
-- Supplement logs on local days from first to last, oldest first.
SELECT * FROM supplement_logs
WHERE user_id = @user_id AND local_date BETWEEN @first AND @last AND deleted_at IS NULL
ORDER BY local_date, id;
