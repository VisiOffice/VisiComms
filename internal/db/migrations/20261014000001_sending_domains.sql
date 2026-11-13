-- Domains a workspace sends as. Mail goes out through the platform's SES
-- account, which signs for a domain once its DKIM records are in place. That
-- account is shared, so SES already trusting a domain says nothing about which
-- workspace owns it: each workspace also proves ownership with its own TXT
-- record before it can send as the domain.
CREATE TABLE sending_domains (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    domain text NOT NULL CHECK (domain = lower(domain)),
    -- Published by the business as _visicomms.<domain> TXT "visicomms-verify=<token>".
    ownership_token text NOT NULL,
    ownership_verified_at timestamptz,
    -- SES's own state for the identity: PENDING, SUCCESS, FAILED, ...
    ses_status text NOT NULL DEFAULT 'PENDING',
    dkim_tokens text[] NOT NULL DEFAULT '{}',
    checked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, domain)
);

-- Per-connector settings that are not secrets: for SES, the From name and
-- address. Credentials stay in credentials_ciphertext (or the platform key).
ALTER TABLE connectors ADD COLUMN settings jsonb NOT NULL DEFAULT '{}';
