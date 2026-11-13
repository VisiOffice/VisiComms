-- Postal mail out, through the workspace's own Lob account.

-- A postal identity's address, in parts. identities.address holds Lob's
-- standardised one-line form, so the existing unique index stops duplicates.
CREATE TABLE postal_addresses (
    identity_id uuid PRIMARY KEY REFERENCES identities ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 40),
    company text CHECK (length(company) <= 40),
    line1 text NOT NULL CHECK (length(line1) BETWEEN 1 AND 64),
    line2 text CHECK (length(line2) <= 64),
    city text NOT NULL,
    state text NOT NULL,
    zip text NOT NULL,
    country text NOT NULL DEFAULT 'US' CHECK (country = 'US'),
    -- Lob's US verification result. It says USPS can deliver there, not
    -- that the address belongs to the contact: identities.verified is
    -- still a person's call.
    deliverability text NOT NULL,
    checked_at timestamptz NOT NULL DEFAULT now(),
    -- A letter to it came back. No more letters go until a person edits or
    -- re-confirms it.
    returned_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX postal_addresses_workspace ON postal_addresses (workspace_id);

-- One postal thread per contact, whatever address each letter went to.
CREATE UNIQUE INDEX threads_postal_contact ON threads (workspace_id, contact_id) WHERE channel = 'postal';

-- One Lob account per workspace.
CREATE UNIQUE INDEX connectors_lob_workspace ON connectors (workspace_id) WHERE kind = 'lob';
