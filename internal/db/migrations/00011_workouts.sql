-- +goose Up
-- Whether the user worked out: one row per log, naming the sessions it
-- covers. Whether a session was planned is worked out from the training
-- plan for the day, so a session not planned that day is an extra. A
-- deleted row is kept, so "undo" can bring it back.
CREATE TABLE workouts (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The action that created it, and the text that action answered.
    action_id  BIGINT NOT NULL REFERENCES actions (id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages (id),
    -- The user's local day it happened.
    local_date DATE NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('done', 'skipped')),
    -- Session names, like {legs,cardio}. A session is logged at most once
    -- a day, which code checks.
    sessions   TEXT[] NOT NULL CHECK (cardinality(sessions) > 0),
    logged_at  TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX workouts_user_day ON workouts (user_id, local_date) WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE workouts;
