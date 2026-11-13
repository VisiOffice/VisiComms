-- Pop-up messages and quick replies for web chat.

-- Buttons shown under the greeting; tapping one sends its words.
ALTER TABLE chat_widgets ADD COLUMN quick_replies text[] NOT NULL DEFAULT '{}';

-- A message the widget shows a visitor who has not written yet. It is held,
-- not stored: nothing about the visitor is saved when one is shown. When the
-- visitor replies, the pop-up's text is written into the new thread first.
CREATE TABLE chat_popups (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    widget_id uuid NOT NULL REFERENCES chat_widgets,
    -- Lower shows first when several are due at once.
    position integer NOT NULL DEFAULT 0,
    name text NOT NULL,
    text text NOT NULL,
    -- Who it appears to be from: the widget's name, or a set name.
    sender_name text,
    -- delay: seconds on the page; page: path matches page_pattern (after
    -- seconds); exit: pointer leaves toward the browser bar; scroll: scrolled
    -- scroll_percent of the page; idle: no activity for seconds.
    trigger text NOT NULL CHECK (trigger IN ('delay', 'page', 'exit', 'scroll', 'idle')),
    seconds integer NOT NULL DEFAULT 0 CHECK (seconds BETWEEN 0 AND 3600),
    scroll_percent integer NOT NULL DEFAULT 50 CHECK (scroll_percent BETWEEN 1 AND 100),
    page_pattern text,
    opens_window boolean NOT NULL DEFAULT false,
    sound boolean NOT NULL DEFAULT false,
    devices text NOT NULL DEFAULT 'all' CHECK (devices IN ('all', 'desktop', 'mobile')),
    frequency text NOT NULL DEFAULT 'session' CHECK (frequency IN ('session', 'first_visit')),
    -- Only if no other pop-up has shown this visit.
    only_if_none boolean NOT NULL DEFAULT false,
    quick_replies text[] NOT NULL DEFAULT '{}',
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_popups_widget ON chat_popups (widget_id, position);

-- How often each pop-up is shown and how often a chat follows, per day,
-- counted without storing anything about the visitor.
CREATE TABLE chat_popup_stats (
    popup_id uuid NOT NULL REFERENCES chat_popups ON DELETE CASCADE,
    day date NOT NULL,
    shown integer NOT NULL DEFAULT 0,
    chats integer NOT NULL DEFAULT 0,
    PRIMARY KEY (popup_id, day)
);
