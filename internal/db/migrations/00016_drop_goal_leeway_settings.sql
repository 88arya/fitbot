-- +goose Up
-- The calorie floor and goal band are gone, so their rows mean nothing.
DELETE FROM settings WHERE key IN ('calorie_floor', 'goal_band');

-- +goose Down
SELECT 1;
