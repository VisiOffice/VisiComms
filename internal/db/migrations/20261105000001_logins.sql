-- A login is one human across workspaces. Each workspace still has its own
-- principal for that human (its own name, permissions and audit trail); the
-- login links them so a signed-in person can switch workspace without
-- another passkey. Logins are linked only by accepting an invite while
-- signed in, or by signing in with the other workspace's passkey.
CREATE TABLE logins (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE principals ADD COLUMN login_id uuid;
UPDATE principals SET login_id = gen_random_uuid() WHERE kind = 'human';
INSERT INTO logins (id) SELECT login_id FROM principals WHERE login_id IS NOT NULL;
ALTER TABLE principals ADD CONSTRAINT principals_login_fk FOREIGN KEY (login_id) REFERENCES logins;
-- One principal per login per workspace.
CREATE UNIQUE INDEX principals_login_workspace ON principals (login_id, workspace_id) WHERE login_id IS NOT NULL;
