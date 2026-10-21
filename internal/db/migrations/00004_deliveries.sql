-- +goose Up
-- Texts Fitbot sends on its own schedule, one row per user, kind, and local day.
CREATE TABLE deliveries (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- What is being sent, like morning_checkin.
    kind         TEXT NOT NULL,
    -- The user's local day the text is for. Part of the unique key, so a day
    -- is never sent twice even if the time or timezone changes.
    local_date   DATE NOT NULL,
    -- When the text is meant to go out; lateness is measured from here.
    scheduled_at TIMESTAMPTZ NOT NULL,
    -- When to try next; later than scheduled_at after a failed attempt.
    run_at       TIMESTAMPTZ NOT NULL,
    -- pending: waiting for run_at. sending: claimed by a worker.
    -- sent: Telnyx accepted it. skipped: too late, or no longer a recipient.
    -- failed: not sent, and no more tries. unknown: a send may or may not
    -- have gone out, so it is never retried.
    status       TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sending', 'sent', 'skipped', 'failed', 'unknown')),
    attempts     INT NOT NULL DEFAULT 0,
    -- The outbound text, stored before calling Telnyx.
    message_id   BIGINT REFERENCES messages (id),
    error        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, kind, local_date)
);

CREATE INDEX deliveries_due ON deliveries (run_at) WHERE status = 'pending';
CREATE INDEX deliveries_sending ON deliveries (updated_at) WHERE status = 'sending';

-- +goose Down
DROP TABLE deliveries;
