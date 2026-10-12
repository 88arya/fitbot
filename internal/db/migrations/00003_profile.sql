-- +goose Up
ALTER TABLE users
    ADD COLUMN name TEXT,
    -- What the user wants to spend running Spotter each month (texts and LLM).
    ADD COLUMN monthly_budget_usd NUMERIC(10, 2) CHECK (monthly_budget_usd >= 0),
    -- NULL until the user gives one, so onboarding knows to ask.
    ALTER COLUMN timezone DROP NOT NULL;

-- Existing timezones came from DEFAULT_TIMEZONE, not from the user.
UPDATE users SET timezone = NULL;

-- Facts the user has told Spotter, kept as sentences the LLM reads with every request.
CREATE TABLE memories (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    fact       TEXT NOT NULL CHECK (length(fact) BETWEEN 1 AND 300),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX memories_user ON memories (user_id, id);

-- +goose Down
DROP TABLE memories;
UPDATE users SET timezone = 'UTC' WHERE timezone IS NULL;
ALTER TABLE users
    DROP COLUMN monthly_budget_usd,
    DROP COLUMN name,
    ALTER COLUMN timezone SET NOT NULL;
