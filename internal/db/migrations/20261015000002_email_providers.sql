-- Keep SES identities while permitting the same domain on another provider.
ALTER TABLE sending_domains ADD COLUMN provider text NOT NULL DEFAULT 'ses'
    CHECK (provider IN ('ses', 'resend'));
ALTER TABLE sending_domains RENAME COLUMN ses_status TO provider_status;
ALTER TABLE sending_domains ADD COLUMN dns_records jsonb NOT NULL DEFAULT '[]';
ALTER TABLE sending_domains DROP CONSTRAINT sending_domains_workspace_id_domain_key;
ALTER TABLE sending_domains ADD UNIQUE (workspace_id, provider, domain);
