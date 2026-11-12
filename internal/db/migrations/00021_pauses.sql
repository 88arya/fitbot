-- +goose Up
-- Breaks like a vacation or a sick week. A paused day plans no sessions,
-- isn't judged against goals or counted in streaks, and gets no scheduled
-- texts but supplement reminders. A removed or replaced pause is kept, so
-- "undo" can bring it back.
CREATE TABLE pauses (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The user's local days it covers, both included.
    first_date DATE NOT NULL,
    last_date  DATE NOT NULL,
    -- What the user called it, like "vacation", if they said.
    reason     TEXT,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (last_date >= first_date)
);

CREATE INDEX pauses_user_days ON pauses (user_id, first_date) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE pauses;
