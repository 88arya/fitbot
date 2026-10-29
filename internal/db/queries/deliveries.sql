-- name: PlanDelivery :exec
-- Adds the delivery for a user's local day and slot. A pending one that has
-- not been tried yet moves to the new time, like after a timezone change.
INSERT INTO deliveries (user_id, kind, local_date, slot, scheduled_at, run_at, updated_at)
VALUES (@user_id, @kind, @local_date, @slot, @scheduled_at, @scheduled_at, @now)
ON CONFLICT (user_id, kind, local_date, slot) DO UPDATE
SET scheduled_at = EXCLUDED.scheduled_at, run_at = EXCLUDED.run_at, updated_at = EXCLUDED.updated_at
WHERE deliveries.status = 'pending'
  AND deliveries.attempts = 0
  AND deliveries.scheduled_at <> EXCLUDED.scheduled_at;

-- name: ClaimDueDeliveries :many
-- Marks due deliveries as sending. SKIP LOCKED lets several workers claim
-- at once without taking the same row.
UPDATE deliveries
SET status = 'sending', attempts = attempts + 1, updated_at = @now
WHERE id IN (
    SELECT id FROM deliveries
    WHERE status = 'pending' AND run_at <= @now
    ORDER BY run_at, id
    LIMIT @max_rows
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: LinkDeliveryMessage :execrows
-- Matches only while the claim is still this worker's: reconcile or another
-- worker may have taken over a claim that stalled.
UPDATE deliveries SET message_id = @message_id, asks = @asks, updated_at = @now
WHERE id = @id AND status = 'sending' AND attempts = @attempts;

-- name: FinishDelivery :exec
UPDATE deliveries SET status = @status, error = sqlc.narg(error), updated_at = @now
WHERE id = @id;

-- name: RetryDelivery :exec
UPDATE deliveries
SET status = 'pending', run_at = @run_at, message_id = NULL, error = @error, updated_at = @now
WHERE id = @id;

-- name: ReconcileStaleDeliveries :many
-- Settles deliveries a worker claimed but never finished, like after a
-- crash. Without a stored text nothing was sent, so it goes back to pending.
-- A text Telnyx returned an ID for was sent. Anything else may have gone
-- out, so it is not retried.
UPDATE deliveries d
SET status = CASE
        WHEN d.message_id IS NULL THEN 'pending'
        WHEN EXISTS (SELECT 1 FROM messages m WHERE m.id = d.message_id AND m.provider_id IS NOT NULL) THEN 'sent'
        ELSE 'unknown'
    END,
    updated_at = @now
WHERE d.status = 'sending' AND d.updated_at < @cutoff
RETURNING *;

-- name: DeliveryBefore :one
-- The scheduled text that is the user's newest outbound message before
-- message before_id, if that message is one.
SELECT d.* FROM deliveries d
WHERE d.user_id = @user_id AND d.message_id = (
    SELECT m.id FROM messages m
    WHERE m.user_id = @user_id AND m.direction = 'outbound' AND m.id < @before_id
    ORDER BY m.id DESC
    LIMIT 1
);
