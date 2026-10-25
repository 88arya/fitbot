-- name: ClaimAction :one
-- Returns no rows when the call was already applied for this message. A
-- concurrent run of the same message waits here until the first commits.
INSERT INTO actions (user_id, message_id, tool, args, occurrence, result)
VALUES (@user_id, @message_id, @tool, @args, @occurrence, '')
ON CONFLICT (message_id, tool, args, occurrence) DO NOTHING
RETURNING id;

-- name: SetActionResult :exec
UPDATE actions SET result = @result WHERE id = @id;

-- name: ActionResult :one
SELECT result FROM actions
WHERE message_id = @message_id AND tool = @tool AND args = @args AND occurrence = @occurrence;
