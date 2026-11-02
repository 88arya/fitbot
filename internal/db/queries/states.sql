-- name: SetBodyState :exec
INSERT INTO body_states (user_id, state, start_date, tier, calorie_delta, protein_g, fat_g)
VALUES (@user_id, @state, @start_date, sqlc.narg(tier), sqlc.narg(calorie_delta), sqlc.narg(protein_g), sqlc.narg(fat_g))
ON CONFLICT (user_id, start_date) DO UPDATE
SET state = EXCLUDED.state, tier = EXCLUDED.tier, calorie_delta = EXCLUDED.calorie_delta,
    protein_g = EXCLUDED.protein_g, fat_g = EXCLUDED.fat_g, updated_at = now();

-- name: BodyStateOnDay :one
-- The state row that starts on day, if a change was made that day.
SELECT * FROM body_states WHERE user_id = @user_id AND start_date = @start_date;

-- name: DeleteBodyState :exec
DELETE FROM body_states WHERE user_id = @user_id AND start_date = @start_date;

-- name: BodyStateHistory :many
-- Every state row, oldest first, so a range of days can each be judged by
-- the state in force then.
SELECT * FROM body_states WHERE user_id = @user_id ORDER BY start_date;
