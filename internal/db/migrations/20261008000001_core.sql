-- Core records for the phase 1 email pilot.
-- Current state lives in ordinary tables; events are the append-only history,
-- written in the same transaction as the state they describe.

CREATE TABLE workspaces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    timezone text NOT NULL,
    -- {"mon": [["08:00","17:00"]], ...} in the workspace timezone.
    business_hours jsonb NOT NULL DEFAULT '{}',
    -- Workspace kill switch: when set, nothing is dispatched.
    sending_paused_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Anyone who can act: a human user or an agent.
CREATE TABLE principals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    kind text NOT NULL CHECK (kind IN ('human', 'agent')),
    name text NOT NULL,
    email text,
    -- Agents authenticate with a token; only its SHA-256 is stored.
    token_sha256 bytea UNIQUE,
    scopes text[] NOT NULL DEFAULT '{}',
    autonomy text NOT NULL DEFAULT 'approve' CHECK (autonomy IN ('observe', 'approve', 'autonomous')),
    -- Prompt, tool set and model of an agent; a change re-opens promotion review.
    config_version text,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (kind = 'agent' OR token_sha256 IS NULL)
);
CREATE INDEX principals_workspace ON principals (workspace_id);

CREATE TABLE contacts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    display_name text,
    tags text[] NOT NULL DEFAULT '{}',
    owner_id uuid REFERENCES principals,
    -- Derived from what the contact wrote: untrusted, never approved info.
    memory jsonb NOT NULL DEFAULT '[]',
    -- Authority field: humans only.
    trusted boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX contacts_workspace ON contacts (workspace_id);

-- One way to reach a contact.
CREATE TABLE identities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    contact_id uuid NOT NULL REFERENCES contacts,
    channel text NOT NULL CHECK (channel IN ('email', 'sms', 'voice', 'postal')),
    -- Normalised: lower-cased email, E.164 phone.
    address text NOT NULL,
    -- Authority field: humans and verification checks only.
    verified boolean NOT NULL DEFAULT false,
    confidence real NOT NULL DEFAULT 1,
    merged_from uuid REFERENCES contacts,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, channel, address)
);
CREATE INDEX identities_contact ON identities (contact_id);

-- Per-channel opt-out, set by the inbound handler itself (STOP, unsubscribe)
-- or by a human; never by an agent.
CREATE TABLE opt_outs (
    contact_id uuid NOT NULL REFERENCES contacts,
    channel text NOT NULL CHECK (channel IN ('email', 'sms', 'voice', 'postal')),
    source text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (contact_id, channel)
);

-- A customer's provider account.
CREATE TABLE connectors (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    kind text NOT NULL,
    name text NOT NULL,
    -- Encrypted with the workspace data key; never returned by any API.
    credentials_ciphertext bytea,
    health text NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown', 'ok', 'failing')),
    -- The provider's own error text, in plain words, and who must fix it.
    last_error text,
    fix_owner text,
    checked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX connectors_workspace ON connectors (workspace_id);

CREATE TABLE threads (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    contact_id uuid NOT NULL REFERENCES contacts,
    channel text NOT NULL CHECK (channel IN ('email', 'sms', 'voice', 'postal', 'chat')),
    subject text,
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'waiting', 'resolved')),
    assignee_id uuid REFERENCES principals,
    -- Take-over: agents may read and draft but not send until handed back.
    taken_over_by uuid REFERENCES principals,
    taken_over_at timestamptz,
    -- When a substantive reply is due (pilot: 1 business day or the
    -- customer's requested delivery date, whichever is sooner).
    response_due_at timestamptz,
    -- Bumped on every change a run must notice before sending.
    version bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX threads_workspace_state ON threads (workspace_id, state, response_due_at);
CREATE INDEX threads_contact ON threads (contact_id);

-- Facts the business stands behind. A version is immutable once approved,
-- and only a human approves one.
CREATE TABLE approved_info_versions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    version integer NOT NULL,
    content jsonb NOT NULL,
    created_by uuid REFERENCES principals,
    approved_by uuid REFERENCES principals,
    approved_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, version),
    CHECK ((approved_by IS NULL) = (approved_at IS NULL))
);

CREATE FUNCTION approved_info_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.approved_at IS NOT NULL THEN
        RAISE EXCEPTION 'approved info version % is approved and cannot be deleted', OLD.version;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.approved_at IS NOT NULL THEN
        RAISE EXCEPTION 'approved info version % is approved and cannot be changed', OLD.version;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.approved_by IS NOT NULL
        AND (SELECT kind FROM principals WHERE id = NEW.approved_by) <> 'human' THEN
        RAISE EXCEPTION 'only a human can approve approved info';
    END IF;
    RETURN COALESCE(NEW, OLD);
END $$;

CREATE TRIGGER approved_info_frozen BEFORE UPDATE OR DELETE ON approved_info_versions
    FOR EACH ROW EXECUTE FUNCTION approved_info_frozen();

-- Anything that happened. Append-only: corrections are new events that point
-- at the old one.
CREATE TABLE events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces,
    thread_id uuid REFERENCES threads,
    contact_id uuid REFERENCES contacts,
    type text NOT NULL,
    actor_id uuid REFERENCES principals,
    channel text,
    payload jsonb NOT NULL DEFAULT '{}',
    -- Provider and its id for the thing (Message-ID, SES id): inbound dedupe.
    provider text,
    provider_ref text,
    -- Written by a contact: fenced and labelled whenever shown to a model.
    untrusted boolean NOT NULL DEFAULT false,
    suspected_injection boolean NOT NULL DEFAULT false,
    -- Files live in the customer's storage; the event keeps pointer and hash.
    storage_url text,
    storage_sha256 bytea,
    -- source, campaign, keyword, landing_page, each with how it was obtained;
    -- any of them may be unknown.
    attribution jsonb,
    corrects_event_id bigint REFERENCES events,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_thread ON events (thread_id, id);
CREATE INDEX events_workspace ON events (workspace_id, id);
CREATE UNIQUE INDEX events_provider_ref ON events (workspace_id, provider, provider_ref)
    WHERE provider_ref IS NOT NULL;

CREATE FUNCTION events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'events are append-only: record a correcting event instead of %', TG_OP;
END $$;

CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_append_only();

-- A pointer to a business record that lives elsewhere (a dispatch system's QuoteRequest,
-- Quote, Order). State is pushed in; sends that depend on missing or stale
-- state are held.
CREATE TABLE links (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    thread_id uuid REFERENCES threads,
    contact_id uuid REFERENCES contacts,
    system text NOT NULL,
    record_type text NOT NULL,
    external_id text NOT NULL,
    state text,
    state_payload jsonb NOT NULL DEFAULT '{}',
    pushed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (thread_id IS NOT NULL OR contact_id IS NOT NULL),
    UNIQUE NULLS NOT DISTINCT (workspace_id, system, record_type, external_id, thread_id)
);
CREATE INDEX links_thread ON links (thread_id);

-- An action an agent wants to take that policy held. The approval covers
-- exactly what payload_sha256 covers; a change to any part voids it.
CREATE TABLE proposals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    thread_id uuid REFERENCES threads,
    requested_by uuid NOT NULL REFERENCES principals,
    action text NOT NULL,
    payload jsonb NOT NULL,
    recipients jsonb NOT NULL DEFAULT '[]',
    -- Linked record states the draft was composed against.
    linked_states jsonb NOT NULL DEFAULT '[]',
    approved_info_version_id uuid REFERENCES approved_info_versions,
    -- SHA-256 over recipients, payload, linked states and approved info version.
    payload_sha256 bytea NOT NULL,
    rule text NOT NULL,
    reason text,
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'voided')),
    decided_by uuid REFERENCES principals,
    decided_at timestamptz,
    decision_note text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status IN ('approved', 'rejected')) = (decided_by IS NOT NULL))
);
CREATE INDEX proposals_pending ON proposals (workspace_id, created_at) WHERE status = 'pending';
CREATE INDEX proposals_thread ON proposals (thread_id);

-- One outbound action to carry out, approved or allowed. Only the executor
-- dispatches it.
CREATE TABLE actions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces,
    thread_id uuid REFERENCES threads,
    proposal_id uuid UNIQUE REFERENCES proposals,
    requested_by uuid NOT NULL REFERENCES principals,
    connector_id uuid REFERENCES connectors,
    action text NOT NULL,
    payload jsonb NOT NULL,
    recipients jsonb NOT NULL DEFAULT '[]',
    payload_sha256 bytea NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN
        ('queued', 'dispatching', 'accepted', 'delivered', 'failed', 'unknown', 'cancelled')),
    idempotency_key text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX actions_open ON actions (workspace_id, state)
    WHERE state IN ('queued', 'dispatching', 'unknown', 'failed');
CREATE INDEX actions_thread ON actions (thread_id);

-- One call to a provider for an action.
CREATE TABLE delivery_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action_id uuid NOT NULL REFERENCES actions,
    connector_id uuid REFERENCES connectors,
    sent_at timestamptz NOT NULL DEFAULT now(),
    provider_response jsonb,
    provider_message_id text,
    outcome text NOT NULL CHECK (outcome IN ('accepted', 'rejected', 'error', 'unknown'))
);
CREATE INDEX delivery_attempts_action ON delivery_attempts (action_id);
CREATE INDEX delivery_attempts_provider_message ON delivery_attempts (provider_message_id)
    WHERE provider_message_id IS NOT NULL;
