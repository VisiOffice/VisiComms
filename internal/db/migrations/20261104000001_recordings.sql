-- Call recordings and voicemails, copied off SignalWire into the
-- installation's own bucket and deleted everywhere a year after the call.
-- Rows are found from the call.recorded and voice.voicemail events, so
-- recordings made before this table existed are archived too.
CREATE TABLE recordings (
    call_id uuid PRIMARY KEY REFERENCES calls,
    workspace_id uuid NOT NULL REFERENCES workspaces,
    -- When SignalWire reported the recording; retention counts from here.
    recorded_at timestamptz NOT NULL,
    provider_url text NOT NULL,
    -- Set once the copy is in the bucket.
    object_key text,
    object_version text,
    content_type text,
    bytes bigint,
    sha256 bytea,
    archived_at timestamptz,
    -- SignalWire's copy is deleted once ours is safe, or at expiry.
    provider_deleted_at timestamptz,
    -- Both copies deleted a year after the call.
    expired_at timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text
);
CREATE INDEX recordings_pending ON recordings (next_attempt_at) WHERE expired_at IS NULL;
CREATE INDEX events_recordings ON events (occurred_at) WHERE type IN ('call.recorded', 'voice.voicemail');
