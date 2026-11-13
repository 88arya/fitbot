-- +goose Up
-- An inbound text's status is now received until its reply is sent, then
-- answered, or missed when it was left unanswered too long to answer after
-- a restart. Texts from before this were all answered or lost already.
UPDATE messages SET status = 'answered' WHERE direction = 'inbound' AND status = 'received';

CREATE INDEX messages_unanswered ON messages (id) WHERE direction = 'inbound' AND status = 'received';

-- +goose Down
DROP INDEX messages_unanswered;
UPDATE messages SET status = 'received' WHERE direction = 'inbound';
