-- +goose Up
-- What the calorie estimate needs. Weight comes from weight_logs. Age is kept
-- with the day it was given, so it can be worked out later.
ALTER TABLE users
    ADD COLUMN height_mm INT CHECK (height_mm BETWEEN 1000 AND 2500),
    ADD COLUMN age INT CHECK (age BETWEEN 13 AND 100),
    ADD COLUMN age_on DATE,
    ADD COLUMN sex TEXT CHECK (sex IN ('male', 'female')),
    ADD COLUMN activity TEXT CHECK (activity IN ('sedentary', 'light', 'moderate', 'active', 'very_active'));

-- How the state is run. tier is a preset, custom, or manual (the user sets
-- their own goals). calorie_delta is the daily change from maintenance, and
-- NULL with no tier means the rate hasn't been chosen. protein_g and fat_g
-- are amounts the user set, which targets keep.
ALTER TABLE body_states
    ADD COLUMN tier TEXT CHECK (tier IN ('light', 'moderate', 'aggressive', 'extreme', 'lean', 'standard', 'dirty', 'custom', 'manual')),
    ADD COLUMN calorie_delta INT CHECK (calorie_delta BETWEEN -1000 AND 1000),
    ADD COLUMN protein_g INT,
    ADD COLUMN fat_g INT;

-- Existing users set their own goals; they keep them until they choose a rate.
UPDATE body_states SET tier = 'manual';

-- Every time code changed the targets, and why. goals holds the values the
-- trackers read; this holds how they were worked out.
CREATE TABLE target_changes (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    local_date    DATE NOT NULL,
    reason        TEXT NOT NULL CHECK (reason IN ('plan', 'profile', 'goal', 'review', 'undo')),
    -- Says why in words, with the weight trend for a review.
    detail        TEXT NOT NULL,
    state         TEXT NOT NULL,
    tier          TEXT,
    calorie_delta INT NOT NULL,
    bmr           INT NOT NULL,
    -- The formula's maintenance, and what weekly reviews learned on top of it.
    tdee          INT NOT NULL,
    adjustment    INT NOT NULL,
    calories      INT NOT NULL,
    protein       INT NOT NULL,
    carbs         INT NOT NULL,
    fat           INT NOT NULL,
    weight_grams  INT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX target_changes_user ON target_changes (user_id, id DESC);

-- One row per user and reviewed week, so a review runs once however many
-- times its text is built.
CREATE TABLE target_reviews (
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    week_start DATE NOT NULL,
    outcome    TEXT NOT NULL CHECK (outcome IN ('on_track', 'unknown', 'watching', 'held', 'changed')),
    -- The text it sends; empty for none.
    body       TEXT NOT NULL,
    change_id  BIGINT REFERENCES target_changes (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, week_start)
);

-- +goose Down
DROP TABLE target_reviews;
DROP TABLE target_changes;
ALTER TABLE body_states
    DROP COLUMN tier,
    DROP COLUMN calorie_delta,
    DROP COLUMN protein_g,
    DROP COLUMN fat_g;
ALTER TABLE users
    DROP COLUMN height_mm,
    DROP COLUMN age,
    DROP COLUMN age_on,
    DROP COLUMN sex,
    DROP COLUMN activity;
