-- OAuth for the MCP server, so it can be
-- added as a connector in Claude and other hosted clients. Clients register
-- themselves (RFC 7591); a person approves each one in the console for the
-- workspaces they pick, and the approval makes one agent in each.

CREATE TABLE oauth_clients (
    id text PRIMARY KEY,
    -- Chosen by whoever registered; shown on the consent page as theirs.
    name text NOT NULL,
    redirect_uris text[] NOT NULL,
    -- Null for a public client, which proves itself with PKCE alone.
    secret_sha256 bytea,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- An authorization request waiting for a person in the console.
CREATE TABLE oauth_requests (
    id text PRIMARY KEY,
    client_id text NOT NULL REFERENCES oauth_clients ON DELETE CASCADE,
    redirect_uri text NOT NULL,
    state text,
    code_challenge text NOT NULL,
    expires_at timestamptz NOT NULL
);

-- What a person approved: one client, acting as one agent in each
-- workspace they picked (principals.oauth_grant_id).
CREATE TABLE oauth_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id text NOT NULL REFERENCES oauth_clients ON DELETE CASCADE,
    approved_by uuid NOT NULL REFERENCES principals,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
ALTER TABLE principals ADD COLUMN oauth_grant_id uuid REFERENCES oauth_grants;
CREATE INDEX principals_oauth_grant ON principals (oauth_grant_id) WHERE oauth_grant_id IS NOT NULL;

CREATE TABLE oauth_codes (
    code_sha256 bytea PRIMARY KEY,
    grant_id uuid NOT NULL REFERENCES oauth_grants ON DELETE CASCADE,
    redirect_uri text NOT NULL,
    code_challenge text NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at timestamptz
);

-- Access and refresh tokens; only their SHA-256 is stored.
CREATE TABLE oauth_tokens (
    token_sha256 bytea PRIMARY KEY,
    grant_id uuid NOT NULL REFERENCES oauth_grants ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('access', 'refresh')),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oauth_tokens_grant ON oauth_tokens (grant_id);
