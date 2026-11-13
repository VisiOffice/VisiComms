-- People who are online in the console's phone, so "ring in the app" rings
-- only browsers that can answer. The console checks in every 30 seconds.
CREATE TABLE phone_presence (
    principal_id uuid PRIMARY KEY REFERENCES principals,
    workspace_id uuid NOT NULL REFERENCES workspaces,
    online boolean NOT NULL,
    seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX phone_presence_workspace ON phone_presence (workspace_id) WHERE online;
