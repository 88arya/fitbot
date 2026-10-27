-- +goose Up
-- Nights of sleep, one per local day the user woke up on. A deleted row is
-- kept, so "undo" can bring it back.
CREATE TABLE sleep_logs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The action that created it, and the text that action answered.
    action_id  BIGINT NOT NULL REFERENCES actions (id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages (id),
    -- The user's local day they woke up, which the night counts toward.
    local_date DATE NOT NULL,
    minutes    INT NOT NULL CHECK (minutes > 0 AND minutes <= 24 * 60),
    -- Local times, when the user gave them.
    bedtime    TIME,
    wake_time  TIME,
    logged_at  TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One night per wake date; a second log for the same night is an edit.
CREATE UNIQUE INDEX sleep_logs_user_day ON sleep_logs (user_id, local_date) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE sleep_logs;
