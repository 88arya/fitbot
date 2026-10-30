-- +goose Up
-- Install-wide settings changed by text. A row overrides the env default;
-- with no row the default applies. Which text changed one is in actions.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_by BIGINT REFERENCES users (id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE settings;
