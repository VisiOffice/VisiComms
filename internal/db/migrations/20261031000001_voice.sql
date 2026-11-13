-- A call flow is what a number does with an incoming call. Every change is a
-- new version; a call records the version it ran. An agent without autonomy
-- writes a pending version, which a person activates.
CREATE TABLE call_flows (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    number text NOT NULL REFERENCES phone_numbers(number),
    version int NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'active', 'superseded', 'discarded')),
    flow jsonb NOT NULL,
    created_by uuid REFERENCES principals,
    activated_by uuid REFERENCES principals,
    created_at timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    UNIQUE (number, version)
);
CREATE UNIQUE INDEX call_flows_active ON call_flows (number) WHERE status = 'active';
CREATE UNIQUE INDEX call_flows_pending ON call_flows (number) WHERE status = 'pending';

CREATE TABLE calls (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    thread_id uuid NOT NULL REFERENCES threads,
    contact_id uuid NOT NULL REFERENCES contacts,
    call_sid text NOT NULL UNIQUE,
    provider_project_id text NOT NULL,
    number text NOT NULL REFERENCES phone_numbers(number),
    remote text NOT NULL,
    flow_id uuid REFERENCES call_flows,
    -- Unset while the call is live; answered, voicemail, missed or failed.
    outcome text CHECK (outcome IN ('answered', 'voicemail', 'missed', 'failed')),
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz
);
CREATE INDEX calls_workspace ON calls (workspace_id, started_at);

CREATE UNIQUE INDEX threads_voice_route ON threads (workspace_id, remote_address, phone_number)
    WHERE channel = 'voice';

