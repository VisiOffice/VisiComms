-- People sign in with passkeys only. A person joins a workspace through an
-- invite, which is where their first passkey is registered.

-- The opaque id a passkey stores as its user handle, so a discoverable login
-- can say who it is without an email.
ALTER TABLE principals ADD COLUMN webauthn_user_id bytea UNIQUE;

CREATE TABLE passkeys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid NOT NULL REFERENCES principals,
    credential_id bytea NOT NULL UNIQUE,
    -- go-webauthn's Credential: public key, sign count, flags, transports.
    credential jsonb NOT NULL,
    name text,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
CREATE INDEX passkeys_principal ON passkeys (principal_id);

CREATE TABLE invites (
    token_sha256 bytea PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces,
    email text NOT NULL,
    name text NOT NULL,
    created_by uuid REFERENCES principals,
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    principal_id uuid REFERENCES principals,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One WebAuthn ceremony in flight: its challenge, used once.
CREATE TABLE auth_ceremonies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL CHECK (kind IN ('register', 'login')),
    session jsonb NOT NULL,
    invite_token_sha256 bytea REFERENCES invites,
    expires_at timestamptz NOT NULL
);

CREATE TABLE sessions (
    token_sha256 bytea PRIMARY KEY,
    principal_id uuid NOT NULL REFERENCES principals,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_principal ON sessions (principal_id);

-- What the policy checked when it held a draft, shown to the reviewer:
-- [{"label": "Price matches approved info v12", "ok": true}, ...].
ALTER TABLE proposals ADD COLUMN checks jsonb NOT NULL DEFAULT '[]';
