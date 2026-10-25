-- +goose Up
-- Every change a tool made, keyed to the inbound text that caused it, so
-- running the same text again returns the stored result instead of applying
-- the change twice.
CREATE TABLE actions (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    message_id BIGINT NOT NULL REFERENCES messages (id),
    tool       TEXT NOT NULL,
    -- The arguments as canonical JSON (sorted keys, no spacing, numbers as
    -- written), compared as text so the key matches the agent's exactly.
    args       TEXT NOT NULL,
    -- Counts identical calls for one text, so "500ml and another 500ml"
    -- applies both.
    occurrence INT NOT NULL,
    -- What the tool returned to the model.
    result     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_id, tool, args, occurrence)
);

CREATE INDEX actions_user ON actions (user_id, id DESC);

-- +goose Down
DROP TABLE actions;
