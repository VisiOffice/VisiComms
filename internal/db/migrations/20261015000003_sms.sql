-- Numbers are instance-provider resources assigned to exactly one workspace.
-- A connector per number keeps the sender fixed for replies and approvals.
CREATE TABLE phone_numbers (
    number text PRIMARY KEY CHECK (number ~ '^\+[1-9][0-9]{6,14}$'),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    connector_id uuid NOT NULL UNIQUE REFERENCES connectors,
    provider_project_id text NOT NULL,
    capabilities jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX phone_numbers_workspace ON phone_numbers (workspace_id);
ALTER TABLE threads ADD COLUMN phone_number text REFERENCES phone_numbers(number);
ALTER TABLE threads ADD COLUMN remote_address text;
CREATE UNIQUE INDEX threads_sms_route ON threads (workspace_id, remote_address, phone_number)
    WHERE channel = 'sms';
