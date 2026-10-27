-- name: InsertSchedule :one
INSERT INTO training_schedules (user_id, kind, cycle_days, start_offset, training_time, start_date)
VALUES (@user_id, sqlc.narg(kind), sqlc.narg(cycle_days), @start_offset, sqlc.narg(training_time), @start_date)
RETURNING *;

-- name: InsertScheduleDay :exec
INSERT INTO schedule_days (schedule_id, slot, sessions, training_time)
VALUES (@schedule_id, @slot, @sessions, sqlc.narg(training_time));

-- name: DeleteScheduleOnDay :exec
-- Removes the schedule that starts on start_date, with its days.
DELETE FROM training_schedules WHERE user_id = @user_id AND start_date = @start_date;

-- name: ScheduleOnDay :one
-- The schedule that starts on start_date, if a change was made that day.
SELECT * FROM training_schedules WHERE user_id = @user_id AND start_date = @start_date;

-- name: LatestSchedule :one
SELECT * FROM training_schedules
WHERE user_id = @user_id
ORDER BY start_date DESC
LIMIT 1;

-- name: SchedulesThrough :many
-- Every schedule starting on or before last, oldest first, so a range of
-- days can each use the schedule in force then.
SELECT * FROM training_schedules
WHERE user_id = @user_id AND start_date <= @last
ORDER BY start_date;

-- name: ScheduleDays :many
SELECT * FROM schedule_days WHERE schedule_id = ANY(@schedule_ids::BIGINT[]) ORDER BY schedule_id, slot;

-- name: PlansBetween :many
-- Day plans from first to last, oldest first.
SELECT * FROM session_plans
WHERE user_id = @user_id AND local_date BETWEEN @first AND @last
ORDER BY local_date;

-- name: MovesBefore :many
-- Moves off days before day that may pause a rotation on or after since:
-- every move whose day is before day and whose target is after since.
SELECT * FROM session_plans
WHERE user_id = @user_id AND status = 'moved' AND local_date < @day AND moved_to > @since
ORDER BY local_date;

-- name: SetPlan :exec
INSERT INTO session_plans (user_id, local_date, status, moved_to, sessions, moved_in, training_time)
VALUES (@user_id, @local_date, sqlc.narg(status), sqlc.narg(moved_to), @sessions, @moved_in, sqlc.narg(training_time))
ON CONFLICT (user_id, local_date) DO UPDATE
SET status = EXCLUDED.status, moved_to = EXCLUDED.moved_to, sessions = EXCLUDED.sessions, moved_in = EXCLUDED.moved_in,
    training_time = EXCLUDED.training_time, updated_at = now();

-- name: DeletePlan :exec
DELETE FROM session_plans WHERE user_id = @user_id AND local_date = @local_date;
