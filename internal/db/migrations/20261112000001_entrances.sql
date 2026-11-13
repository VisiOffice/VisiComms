-- An entrance is a building's door intercom. Its calls run the entrance's own
-- flow, not the dialled number's, so a visitor at the gate is greeted as one.
-- A call is from the entrance when it comes from the entrance's fixed caller
-- ID (an on-site bridge dialling one of the workspace's numbers) or from its
-- SIP account (the intercom calling SignalWire directly).
CREATE TABLE entrances (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    -- Every call from the entrance is on this contact's thread.
    contact_id uuid NOT NULL REFERENCES contacts,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    caller_id text CHECK (caller_id ~ '^\+[1-9][0-9]{6,14}$'),
    flow jsonb NOT NULL,
    -- The SignalWire SIP endpoint the intercom signs in as. The password is
    -- shown once when it is made and never stored.
    sip_username text UNIQUE,
    sip_endpoint_id text,
    provider_project_id text,
    -- Where the intercom signs in, e.g. example.sip.signalwire.com.
    sip_server text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((sip_username IS NULL) = (sip_endpoint_id IS NULL)
       AND (sip_username IS NULL) = (provider_project_id IS NULL)
       AND (sip_username IS NULL) = (sip_server IS NULL))
);
CREATE INDEX entrances_workspace ON entrances (workspace_id);
CREATE UNIQUE INDEX entrances_caller_id ON entrances (workspace_id, caller_id) WHERE caller_id IS NOT NULL;

-- A call from an entrance's SIP account reaches none of the workspace's
-- numbers.
ALTER TABLE calls ALTER COLUMN number DROP NOT NULL;
ALTER TABLE calls ADD COLUMN entrance_id uuid REFERENCES entrances ON DELETE SET NULL;
