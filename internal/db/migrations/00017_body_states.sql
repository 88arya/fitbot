-- +goose Up
-- What the user is aiming for, one row per local day it took effect. A change
-- adds a row instead of overwriting, so a past day is judged by the state in
-- force then.
CREATE TABLE body_states (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    state      TEXT NOT NULL CHECK (state IN ('bulk', 'maintain', 'cut')),
    -- The user's local day the state starts.
    start_date DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A second change on the same day replaces the first.
    UNIQUE (user_id, start_date)
);

-- +goose Down
DROP TABLE body_states;
