ALTER TABLE identities DROP CONSTRAINT identities_channel_check;
ALTER TABLE identities ADD CHECK (channel IN ('email','sms','voice','fax','postal'));
ALTER TABLE opt_outs DROP CONSTRAINT opt_outs_channel_check;
ALTER TABLE opt_outs ADD CHECK (channel IN ('email','sms','voice','fax','postal'));
ALTER TABLE threads DROP CONSTRAINT threads_channel_check;
ALTER TABLE threads ADD CHECK (channel IN ('email','sms','voice','fax','postal','chat'));
CREATE UNIQUE INDEX threads_fax_route ON threads(workspace_id,remote_address,phone_number)
    WHERE channel='fax';

-- Small PDF attachments are included in database backups. Content is immutable
-- so an approval cannot be stretched to cover a different document.
CREATE TABLE documents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    filename text NOT NULL,
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 5 AND 10485760),
    sha256 bytea NOT NULL CHECK (octet_length(sha256)=32),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(workspace_id,id)
);
CREATE TRIGGER documents_immutable BEFORE UPDATE OR DELETE ON documents
    FOR EACH ROW EXECUTE FUNCTION events_append_only();

-- The provider receives an opaque bearer link, never a user's session or a
-- permanent public URL. Only the token hash is stored.
CREATE TABLE document_grants (
    token_sha256 bytea PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces,
    document_id uuid NOT NULL,
    action_id uuid NOT NULL UNIQUE REFERENCES actions,
    expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours',
    FOREIGN KEY(workspace_id,document_id) REFERENCES documents(workspace_id,id)
);
CREATE INDEX documents_workspace ON documents(workspace_id,created_at);
