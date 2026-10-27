-- +goose Up
-- The user's training schedule, one row per local day it took effect. A
-- change adds a row instead of overwriting, so a past day keeps the session
-- it had planned.
CREATE TABLE training_schedules (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- weekly pins sessions to weekdays; rotation repeats a cycle of days.
    -- NULL means the user doesn't follow a schedule.
    kind          TEXT CHECK (kind IN ('weekly', 'rotation')),
    -- Days in the cycle: 7 for weekly.
    cycle_days    INT CHECK (cycle_days BETWEEN 1 AND 28),
    -- For a rotation, the cycle day (from 0) start_date falls on.
    start_offset  INT NOT NULL DEFAULT 0 CHECK (start_offset >= 0),
    -- The usual local training time, for days without their own.
    training_time TIME,
    -- The user's local day the schedule starts.
    start_date    DATE NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A second change on the same day replaces the first.
    UNIQUE (user_id, start_date),
    CHECK ((kind IS NULL) = (cycle_days IS NULL)),
    CHECK (kind <> 'weekly' OR cycle_days = 7)
);

-- The days of a schedule with sessions on them. A day with no row is a rest
-- day.
CREATE TABLE schedule_days (
    schedule_id   BIGINT NOT NULL REFERENCES training_schedules (id) ON DELETE CASCADE,
    -- The weekday (0 is Sunday) for weekly, or the cycle day (from 0) for
    -- a rotation.
    slot          INT NOT NULL CHECK (slot BETWEEN 0 AND 27),
    -- Session names, like {legs,cardio}.
    sessions      TEXT[] NOT NULL CHECK (cardinality(sessions) > 0),
    -- The local training time on this day, if it has its own.
    training_time TIME,
    PRIMARY KEY (schedule_id, slot)
);

-- Changes to one day's plan: a skip, a move, or a time. A move adds the
-- sessions to the day they moved to, for weekly; for a rotation it pauses
-- the cycle until that day instead.
CREATE TABLE session_plans (
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    local_date    DATE NOT NULL,
    status        TEXT CHECK (status IN ('skipped', 'moved')),
    moved_to      DATE CHECK (moved_to > local_date),
    -- The sessions skipped or moved off this day, as planned then.
    sessions      TEXT[] NOT NULL DEFAULT '{}',
    -- Sessions moved here from another day.
    moved_in      TEXT[] NOT NULL DEFAULT '{}',
    training_time TIME,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, local_date),
    CHECK ((status IS NOT DISTINCT FROM 'moved') = (moved_to IS NOT NULL))
);

-- +goose Down
DROP TABLE session_plans;
DROP TABLE schedule_days;
DROP TABLE training_schedules;
