-- +goose Up
-- Foods and drinks the user saved by name, like "usual breakfast" or
-- "bottle", so texting the name logs the same amounts again. Amounts are in
-- canonical units, and NULL means the food has none of that. Saving a name
-- again replaces the row with a new one, and a removed or replaced row is
-- kept, so "undo" can bring it back.
CREATE TABLE saved_foods (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Lowercase, as the user texts it.
    name       TEXT NOT NULL CHECK (name <> '' AND name = lower(name)),
    calories   INT CHECK (calories > 0),
    protein    INT CHECK (protein > 0),
    carbs      INT CHECK (carbs > 0),
    fat        INT CHECK (fat > 0),
    water      INT CHECK (water > 0),
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(calories, protein, carbs, fat, water) > 0)
);

CREATE UNIQUE INDEX saved_foods_user_name ON saved_foods (user_id, name) WHERE deleted_at IS NULL;

-- The saved food an amount was logged from, if any.
ALTER TABLE intake_logs ADD COLUMN saved_food_id BIGINT REFERENCES saved_foods (id);

-- +goose Down
ALTER TABLE intake_logs DROP COLUMN saved_food_id;
DROP TABLE saved_foods;
