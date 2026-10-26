-- +goose Up
-- What reverses each action, so "undo" can step back through them newest
-- first. NULL for actions that cannot be undone.
ALTER TABLE actions
    ADD COLUMN undo      JSONB,
    ADD COLUMN undone_at TIMESTAMPTZ,
    -- The "undo" text that reversed it. Unique, so running that text again
    -- finds the action it already undid instead of undoing another.
    ADD COLUMN undone_by BIGINT UNIQUE REFERENCES messages (id);

-- Amounts the user logged toward a daily total, one row per metric. A day's
-- total is the sum of its rows. A deleted row is kept, so "undo" can bring
-- it back.
CREATE TABLE intake_logs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The action that created it, and the text that action answered.
    action_id  BIGINT NOT NULL REFERENCES actions (id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages (id),
    -- The user's local day it counts toward.
    local_date DATE NOT NULL,
    metric     TEXT NOT NULL CHECK (metric IN ('calories', 'protein', 'carbs', 'fat')),
    -- In canonical units: kcal, or grams for protein, carbs, and fat.
    amount     INT NOT NULL CHECK (amount > 0),
    logged_at  TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX intake_logs_user_day ON intake_logs (user_id, local_date) WHERE deleted_at IS NULL;
CREATE INDEX intake_logs_user_metric ON intake_logs (user_id, metric, id DESC) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE intake_logs;
ALTER TABLE actions
    DROP COLUMN undone_by,
    DROP COLUMN undone_at,
    DROP COLUMN undo;
