-- +goose Up
-- Water is logged like calories: amounts in millilitres added to the day's
-- total.
ALTER TABLE intake_logs
    DROP CONSTRAINT intake_logs_metric_check,
    ADD CONSTRAINT intake_logs_metric_check CHECK (metric IN ('calories', 'protein', 'carbs', 'fat', 'water'));

-- +goose Down
-- Water rows can't pass the old check, so rolling back drops them.
DELETE FROM intake_logs WHERE metric = 'water';
ALTER TABLE intake_logs
    DROP CONSTRAINT intake_logs_metric_check,
    ADD CONSTRAINT intake_logs_metric_check CHECK (metric IN ('calories', 'protein', 'carbs', 'fat'));
