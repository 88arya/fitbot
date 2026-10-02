-- +goose Up
CREATE TABLE users (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    phone      TEXT NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{6,14}$'),
    timezone   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE users;
