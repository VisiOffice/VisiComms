-- Notifications tell people what happened: instantly, in a daily summary, or
-- not at all, per kind of event. A person sets their own in the console; an
-- agent can set them over MCP.
ALTER TABLE principals ADD COLUMN notify_phone text CHECK (notify_phone ~ '^\+[1-9][0-9]{6,14}$');

CREATE TABLE notification_settings (
    principal_id uuid NOT NULL REFERENCES principals,
    category text NOT NULL,
    delivery text NOT NULL CHECK (delivery IN ('instant', 'daily', 'off')),
    -- How an instant notification is sent; a daily summary is always email.
    channels text[] NOT NULL DEFAULT '{email}' CHECK (channels <@ ARRAY['email', 'sms']),
    updated_by uuid REFERENCES principals,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, category)
);

-- The last event the instant notifier has looked at.
CREATE TABLE notification_cursor (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    last_event_id bigint NOT NULL
);
INSERT INTO notification_cursor (last_event_id) SELECT coalesce(max(id), 0) FROM events;

-- One row per notification sent, so a retried run sends nothing twice.
CREATE TABLE notifications_sent (
    principal_id uuid NOT NULL REFERENCES principals,
    event_id bigint NOT NULL,
    channel text NOT NULL,
    sent_at timestamptz NOT NULL DEFAULT now(),
    error text,
    PRIMARY KEY (principal_id, event_id, channel)
);

CREATE TABLE notification_digests (
    principal_id uuid NOT NULL REFERENCES principals,
    day date NOT NULL,
    covered_to bigint NOT NULL,
    sent_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, day)
);
