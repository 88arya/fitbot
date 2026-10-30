-- name: ListSettings :many
SELECT s.key, s.value, s.updated_at, COALESCE(u.name, u.phone, 'a former user')::text AS changed_by
FROM settings s
LEFT JOIN users u ON u.id = s.updated_by
ORDER BY s.key;

-- name: GetSetting :one
SELECT * FROM settings WHERE key = $1;

-- name: SetSetting :exec
INSERT INTO settings (key, value, updated_by, updated_at)
VALUES (@key, @value, @updated_by, @updated_at)
ON CONFLICT (key) DO UPDATE
SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at;

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = $1;
