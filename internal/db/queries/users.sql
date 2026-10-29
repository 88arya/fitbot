-- name: GetUserByPhone :one
SELECT * FROM users WHERE phone = $1;

-- name: UpsertUser :one
INSERT INTO users (phone) VALUES ($1)
ON CONFLICT (phone) DO UPDATE SET phone = EXCLUDED.phone
RETURNING *;

-- name: UpdateProfile :one
-- NULL arguments leave a field unchanged.
UPDATE users
SET name = COALESCE(sqlc.narg(name), name),
    timezone = COALESCE(sqlc.narg(timezone), timezone),
    wake_time = COALESCE(sqlc.narg(wake_time), wake_time),
    bedtime = COALESCE(sqlc.narg(bedtime), bedtime),
    monthly_budget_usd = COALESCE(sqlc.narg(monthly_budget_usd), monthly_budget_usd)
WHERE id = $1
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: ListUsersWithTimezone :many
-- Users whose local time is known, so scheduled texts can be timed for them.
SELECT * FROM users WHERE timezone IS NOT NULL ORDER BY id;

-- name: RestoreProfile :exec
-- Sets the profile fields exactly, NULLs included, to undo a change.
UPDATE users
SET name = sqlc.narg(name),
    timezone = sqlc.narg(timezone),
    wake_time = sqlc.narg(wake_time),
    bedtime = sqlc.narg(bedtime),
    morning_checkin = sqlc.narg(morning_checkin),
    night_checkin = sqlc.narg(night_checkin),
    muted_until = sqlc.narg(muted_until)
WHERE id = @id;

-- name: SetCheckins :exec
-- Sets the check-in times and mute exactly; NULL is the default or unmuted.
UPDATE users
SET morning_checkin = sqlc.narg(morning_checkin),
    night_checkin = sqlc.narg(night_checkin),
    muted_until = sqlc.narg(muted_until)
WHERE id = @id;

-- name: LockUser :one
-- Makes changes to one user's data that must not interleave take turns.
SELECT id FROM users WHERE id = $1 FOR UPDATE;
