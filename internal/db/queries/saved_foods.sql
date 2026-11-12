-- name: ListSavedFoods :many
SELECT * FROM saved_foods
WHERE user_id = @user_id AND deleted_at IS NULL
ORDER BY name;

-- name: CountSavedFoods :one
SELECT count(*) FROM saved_foods WHERE user_id = @user_id AND deleted_at IS NULL;

-- name: GetSavedFood :one
-- The user's saved food with this name, locked so a change to it waits.
SELECT * FROM saved_foods
WHERE user_id = @user_id AND name = @name AND deleted_at IS NULL
FOR UPDATE;

-- name: InsertSavedFood :one
INSERT INTO saved_foods (user_id, name, calories, protein, carbs, fat, water)
VALUES (@user_id, @name, sqlc.narg(calories), sqlc.narg(protein), sqlc.narg(carbs), sqlc.narg(fat), sqlc.narg(water))
RETURNING *;

-- name: SetSavedFoodDeleted :execrows
-- A NULL deleted_at restores the row.
UPDATE saved_foods SET deleted_at = sqlc.narg(deleted_at)
WHERE id = @id AND user_id = @user_id;
