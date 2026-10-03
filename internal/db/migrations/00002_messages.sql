-- +goose Up
CREATE TABLE messages (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users (id),
    direction   TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    body        TEXT NOT NULL,
    -- Telnyx's message ID. Unique so a retried webhook cannot be stored twice.
    -- NULL on an outbound text until the send API returns.
    provider_id TEXT UNIQUE,
    -- received for inbound; sending, then Telnyx's delivery status for outbound.
    status      TEXT NOT NULL,
    parts       INT,
    -- What Telnyx reports it charged, filled in from webhooks.
    cost_usd    NUMERIC(12, 6),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX messages_user_created ON messages (user_id, created_at DESC);

CREATE TABLE llm_calls (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The inbound text that caused the call.
    message_id    BIGINT NOT NULL REFERENCES messages (id),
    model         TEXT NOT NULL,
    input_tokens  INT NOT NULL,
    output_tokens INT NOT NULL,
    latency_ms    INT NOT NULL,
    -- NULL when no price is configured for the model.
    cost_usd      NUMERIC(14, 8),
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE llm_calls;
DROP TABLE messages;
