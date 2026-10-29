-- +goose Up
-- Weigh-ins, in grams. A day can have several; the first one logged is the
-- day's weight. A deleted row is kept, so "undo" can bring it back.
CREATE TABLE weight_logs (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The action that created it, and the text that action answered.
    action_id  BIGINT NOT NULL REFERENCES actions (id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages (id),
    -- The user's local day it was taken.
    local_date DATE NOT NULL,
    grams      INT NOT NULL CHECK (grams > 0),
    logged_at  TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX weight_logs_user_day ON weight_logs (user_id, local_date) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE weight_logs;
