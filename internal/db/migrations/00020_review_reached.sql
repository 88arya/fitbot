-- +goose Up
-- A review can find the target weight reached, which changes nothing and
-- suggests maintain.
ALTER TABLE target_reviews DROP CONSTRAINT target_reviews_outcome_check;
ALTER TABLE target_reviews ADD CONSTRAINT target_reviews_outcome_check
    CHECK (outcome IN ('on_track', 'unknown', 'watching', 'held', 'changed', 'reached'));

-- +goose Down
DELETE FROM target_reviews WHERE outcome = 'reached';
ALTER TABLE target_reviews DROP CONSTRAINT target_reviews_outcome_check;
ALTER TABLE target_reviews ADD CONSTRAINT target_reviews_outcome_check
    CHECK (outcome IN ('on_track', 'unknown', 'watching', 'held', 'changed'));
