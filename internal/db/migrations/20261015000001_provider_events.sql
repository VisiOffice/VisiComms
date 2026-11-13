-- Verified provider notifications are committed before applying their effects.
-- An event can arrive before the executor records the provider message ID.
CREATE TABLE provider_events (
    provider text NOT NULL,
    event_id text NOT NULL,
    message_id text NOT NULL,
    payload jsonb NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    PRIMARY KEY (provider, event_id)
);

CREATE INDEX provider_events_pending ON provider_events (provider, message_id)
    WHERE processed_at IS NULL;
