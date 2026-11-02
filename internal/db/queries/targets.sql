-- name: InsertTargetChange :one
INSERT INTO target_changes (user_id, local_date, reason, detail, state, tier, calorie_delta, bmr, tdee, adjustment,
    calories, protein, carbs, fat, weight_grams)
VALUES (@user_id, @local_date, @reason, @detail, @state, sqlc.narg(tier), @calorie_delta, @bmr, @tdee, @adjustment,
    @calories, @protein, @carbs, @fat, @weight_grams)
RETURNING *;

-- name: LatestTargetChange :one
SELECT * FROM target_changes WHERE user_id = @user_id ORDER BY id DESC LIMIT 1;

-- name: RecentTargetChanges :many
-- The newest changes, newest first, to explain why targets moved.
SELECT * FROM target_changes WHERE user_id = @user_id ORDER BY id DESC LIMIT @count;

-- name: FirstTargetChangeSince :one
-- The first change on or after day, for the weight a plan started from.
SELECT * FROM target_changes WHERE user_id = @user_id AND local_date >= @day ORDER BY id LIMIT 1;

-- name: DeleteTargetChange :exec
DELETE FROM target_changes WHERE id = @id AND user_id = @user_id;

-- name: TargetReview :one
SELECT * FROM target_reviews WHERE user_id = @user_id AND week_start = @week_start;

-- name: LatestTargetReview :one
SELECT * FROM target_reviews WHERE user_id = @user_id ORDER BY week_start DESC LIMIT 1;

-- name: InsertTargetReview :exec
INSERT INTO target_reviews (user_id, week_start, outcome, body, change_id)
VALUES (@user_id, @week_start, @outcome, @body, sqlc.narg(change_id));
