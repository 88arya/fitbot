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
    monthly_budget_usd = COALESCE(sqlc.narg(monthly_budget_usd), monthly_budget_usd)
WHERE id = $1
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;
