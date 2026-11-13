-- Agents sign in with a bearer token whose SHA-256 is principals.token_sha256
-- (core migration). Who made each agent, and when it was last used, are for
-- the console's agent list.
ALTER TABLE principals
    ADD COLUMN created_by uuid REFERENCES principals,
    ADD COLUMN last_used_at timestamptz;
