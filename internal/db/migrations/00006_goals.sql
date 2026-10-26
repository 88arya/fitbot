-- +goose Up
-- Local times of day. NULL until the user gives them, so onboarding knows to
-- ask; scheduled texts use defaults until then.
ALTER TABLE users
    ADD COLUMN wake_time TIME,
    ADD COLUMN bedtime   TIME;

-- The user's goals, one row per metric and local day it took effect. A
-- change adds a row instead of overwriting, so a past day is judged against
-- the goal in force then.
CREATE TABLE goals (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    metric     TEXT NOT NULL
        CHECK (metric IN ('calories', 'protein', 'carbs', 'fat', 'water', 'sleep', 'weight')),
    -- In canonical units: kcal, grams (protein, carbs, fat, and target
    -- weight), millilitres, minutes. Daily, except weight, which is a single
    -- target. NULL means the user chose not to track it.
    value      INT CHECK (value > 0),
    -- The user's local day the goal starts.
    start_date DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A second change on the same day replaces the first.
    UNIQUE (user_id, metric, start_date)
);

-- +goose Down
DROP TABLE goals;
ALTER TABLE users
    DROP COLUMN bedtime,
    DROP COLUMN wake_time;
