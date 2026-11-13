-- One random data key per workspace, sealed with VISICOMMS_MASTER_KEY. It
-- seals connectors.credentials_ciphertext (see internal/secrets).
CREATE TABLE workspace_keys (
    workspace_id uuid PRIMARY KEY REFERENCES workspaces,
    sealed_key bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A workspace's own SignalWire project. Callbacks are routed by project ID, so
-- a project belongs to at most one workspace, and a workspace has one project.
CREATE UNIQUE INDEX connectors_signalwire_project ON connectors ((settings->>'project_id'))
    WHERE kind = 'signalwire_project';
CREATE UNIQUE INDEX connectors_signalwire_project_workspace ON connectors (workspace_id)
    WHERE kind = 'signalwire_project';
