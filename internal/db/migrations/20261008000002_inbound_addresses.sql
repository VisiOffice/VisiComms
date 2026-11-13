-- The address the Cloudflare email worker received a message at selects the
-- workspace. A business's existing inbox forwards to one of these.
CREATE TABLE inbound_addresses (
    address text PRIMARY KEY CHECK (address = lower(address)),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inbound_addresses_workspace ON inbound_addresses (workspace_id);
