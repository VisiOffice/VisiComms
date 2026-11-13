-- A trusted integration (a system of record such as VisiRent, sending its own
-- rent reminders) may send without a person approving each message. Only a
-- person can grant this, per token; it covers sending new messages over REST
-- and nothing else, unlike autonomy = 'autonomous'.
ALTER TABLE principals ADD COLUMN may_send boolean NOT NULL DEFAULT false;
ALTER TABLE principals ADD COLUMN may_send_by uuid REFERENCES principals;
ALTER TABLE principals ADD COLUMN may_send_at timestamptz;
