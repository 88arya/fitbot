-- name: ClaimAction :one
-- Returns no rows when the call was already applied for this message. A
-- concurrent run of the same message waits here until the first commits.
INSERT INTO actions (user_id, message_id, tool, args, occurrence, result)
VALUES (@user_id, @message_id, @tool, @args, @occurrence, '')
ON CONFLICT (message_id, tool, args, occurrence) DO NOTHING
RETURNING id;

-- name: SetActionResult :exec
-- undo is NULL when the change cannot be undone.
UPDATE actions SET result = @result, undo = sqlc.narg(undo) WHERE id = @id;

-- name: ActionResult :one
SELECT result FROM actions
WHERE message_id = @message_id AND tool = @tool AND args = @args AND occurrence = @occurrence;

-- name: UndoneBy :one
-- The action the "undo" text message_id reversed, if it ran before.
SELECT * FROM actions WHERE undone_by = @message_id;

-- name: NextUndo :one
-- The user's newest action that can be undone and isn't yet.
SELECT * FROM actions
WHERE user_id = @user_id AND undo IS NOT NULL AND undone_at IS NULL
ORDER BY id DESC
LIMIT 1;

-- name: MarkUndone :exec
UPDATE actions SET undone_at = now(), undone_by = @message_id WHERE id = @id;

-- name: MessageAction :one
-- The first call of tool the text message_id applied, if any. Rejected
-- calls are rolled back, so only applied ones are found.
SELECT * FROM actions WHERE message_id = @message_id AND tool = @tool ORDER BY id LIMIT 1;
