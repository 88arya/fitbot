-- +goose Up
-- The supplements a user takes, by name. Each one's plan is in
-- supplement_plans.
CREATE TABLE supplements (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Lowercase, like "creatine" or "fish oil".
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

-- A supplement's plan, one row per local day it took effect. A change adds
-- a row instead of overwriting, so a past day keeps the plan it had.
CREATE TABLE supplement_plans (
    supplement_id BIGINT NOT NULL REFERENCES supplements (id) ON DELETE CASCADE,
    start_date    DATE NOT NULL,
    -- false means the user stopped taking it from start_date.
    active        BOOLEAN NOT NULL,
    -- As the user said it, like "5 g" or "2 capsules".
    dose          TEXT,
    -- When it's taken: a local clock time, or the user's wake time or
    -- bedtime. Neither means any time.
    take_at       TIME,
    anchor        TEXT CHECK (anchor IN ('wake', 'bed')),
    -- Weekdays it's taken on, 0 being Sunday. NULL means every day.
    days          INT[] CHECK (days IS NULL OR (cardinality(days) BETWEEN 1 AND 6 AND days <@ ARRAY[0, 1, 2, 3, 4, 5, 6])),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A second change on the same day replaces the first.
    PRIMARY KEY (supplement_id, start_date),
    CHECK (take_at IS NULL OR anchor IS NULL),
    CHECK (active OR (dose IS NULL AND take_at IS NULL AND anchor IS NULL AND days IS NULL))
);

-- Whether a supplement was taken on a day. A due day with no row is missed
-- once it's over. A deleted row is kept, so "undo" can bring it back.
CREATE TABLE supplement_logs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The action that created it, and the text that action answered.
    action_id     BIGINT NOT NULL REFERENCES actions (id) ON DELETE CASCADE,
    message_id    BIGINT NOT NULL REFERENCES messages (id),
    supplement_id BIGINT NOT NULL REFERENCES supplements (id) ON DELETE CASCADE,
    local_date    DATE NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('taken', 'skipped')),
    logged_at     TIMESTAMPTZ NOT NULL,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One log per supplement and day; a second is an edit.
CREATE UNIQUE INDEX supplement_logs_day ON supplement_logs (supplement_id, local_date) WHERE deleted_at IS NULL;
CREATE INDEX supplement_logs_user_day ON supplement_logs (user_id, local_date) WHERE deleted_at IS NULL;

-- Whether setup's supplement question is answered, so "none" counts as an
-- answer.
ALTER TABLE users ADD COLUMN supplements_answered BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users DROP COLUMN supplements_answered;
DROP TABLE supplement_logs;
DROP TABLE supplement_plans;
DROP TABLE supplements;
