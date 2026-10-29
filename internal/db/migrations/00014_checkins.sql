-- +goose Up
-- A kind can send several texts a day, like a supplement reminder for each
-- time. slot tells them apart ('' for a kind sent once a day), and asks is
-- what the text asked about, so a short reply like "done" can answer it.
ALTER TABLE deliveries
    ADD COLUMN slot TEXT NOT NULL DEFAULT '',
    ADD COLUMN asks JSONB,
    DROP CONSTRAINT deliveries_user_id_kind_local_date_key,
    ADD CONSTRAINT deliveries_user_id_kind_local_date_slot_key UNIQUE (user_id, kind, local_date, slot);

CREATE INDEX deliveries_message ON deliveries (message_id);

ALTER TABLE users
    -- Check-in times the user set; NULL follows the wake time and bedtime.
    ADD COLUMN morning_checkin TIME,
    ADD COLUMN night_checkin TIME,
    -- No scheduled texts go out before this.
    ADD COLUMN muted_until TIMESTAMPTZ;

-- +goose Down
ALTER TABLE users
    DROP COLUMN muted_until,
    DROP COLUMN night_checkin,
    DROP COLUMN morning_checkin;

DROP INDEX deliveries_message;
DELETE FROM deliveries WHERE slot <> '';
ALTER TABLE deliveries
    DROP CONSTRAINT deliveries_user_id_kind_local_date_slot_key,
    ADD CONSTRAINT deliveries_user_id_kind_local_date_key UNIQUE (user_id, kind, local_date),
    DROP COLUMN asks,
    DROP COLUMN slot;
