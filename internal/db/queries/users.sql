-- name: GetUserByPhone :one
SELECT * FROM users WHERE phone = $1;

-- name: CreateUser :one
INSERT INTO users (phone, timezone) VALUES ($1, $2)
RETURNING *;

-- name: UpsertUser :one
INSERT INTO users (phone, timezone) VALUES ($1, $2)
ON CONFLICT (phone) DO UPDATE SET phone = EXCLUDED.phone
RETURNING *;
