-- Slack mirroring for web chat. The Slack
-- connection is a connectors row of kind 'slack': settings hold the channel
-- and mention options, credentials_ciphertext the sealed bot token and
-- signing secret.

-- Which Slack thread a chat is copied into, and how far it has been copied.
CREATE TABLE slack_threads (
    thread_id uuid PRIMARY KEY REFERENCES threads,
    connector_id uuid NOT NULL REFERENCES connectors ON DELETE CASCADE,
    channel_id text NOT NULL,
    -- The Slack message that starts the thread; null until it is posted.
    ts text,
    -- The last event copied, or skipped because it came from Slack.
    last_event_id bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX slack_threads_ts ON slack_threads (connector_id, channel_id, ts);

-- A Slack user linked to a workspace member: matched by email the first
-- time, then by this id, because Slack addresses often differ from the one
-- someone signed up with.
ALTER TABLE principals ADD COLUMN slack_user_id text;
CREATE UNIQUE INDEX principals_slack_user ON principals (workspace_id, slack_user_id);
