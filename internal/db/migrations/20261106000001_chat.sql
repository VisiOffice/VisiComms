-- Web chat. A widget is a script tag on a
-- customer's site. A visitor exists only once they have sent a message:
-- opening the widget stores nothing.

CREATE TABLE chat_widgets (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    -- In the script tag, so public. Never used to find a workspace on its own
    -- for anything but this widget's own requests.
    public_id text NOT NULL UNIQUE,
    name text NOT NULL,
    color text NOT NULL DEFAULT '#2563eb' CHECK (color ~ '^#[0-9a-fA-F]{6}$'),
    position text NOT NULL DEFAULT 'right' CHECK (position IN ('right', 'left')),
    greeting text NOT NULL DEFAULT 'Hi! How can we help?',
    returning_greeting text NOT NULL DEFAULT 'Welcome back! How can we help?',
    reply_time text NOT NULL DEFAULT 'We typically reply within an hour.',
    offline_message text NOT NULL DEFAULT 'We''re away right now. Leave a message and how to reach you, and we''ll reply when we''re back.',
    -- Pre-chat form: each field is off, optional or required.
    ask_name text NOT NULL DEFAULT 'off' CHECK (ask_name IN ('off', 'optional', 'required')),
    ask_email text NOT NULL DEFAULT 'off' CHECK (ask_email IN ('off', 'optional', 'required')),
    ask_phone text NOT NULL DEFAULT 'off' CHECK (ask_phone IN ('off', 'optional', 'required')),
    -- Origins (https://example.com) that may embed the widget; empty means
    -- any. Stops accidental embedding only: any client can send any Origin.
    allowed_origins text[] NOT NULL DEFAULT '{}',
    disabled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_widgets_workspace ON chat_widgets (workspace_id);

-- A visitor who has sent at least one message. The token is the visitor's
-- only key to their conversation; only its SHA-256 is stored.
CREATE TABLE chat_visitors (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    widget_id uuid NOT NULL REFERENCES chat_widgets,
    token_sha256 bytea NOT NULL UNIQUE,
    contact_id uuid NOT NULL REFERENCES contacts,
    thread_id uuid NOT NULL UNIQUE REFERENCES threads,
    -- What the visitor typed into the widget or the site passed with
    -- setUser. Unverified, so never added to the contact as an identity:
    -- typing someone else's email must not attach their mail to this contact.
    name text,
    email text,
    phone text,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_visitors_widget ON chat_visitors (widget_id);
