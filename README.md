# VisiComms

VisiComms consolidates business communications so people and agents can work from the same conversations. The intended distribution is open source as part of Visioffice, usable through self-hosting or a paid managed service. Agents propose replies and actions; humans approve, take over or set the rules.

Email, two-way SMS, PDF fax, web chat and human replies are implemented. Voice (call flows, voicemail, AI answering), postal mail through Lob, and an MCP server for agents are in place; the direction below covers what comes next.

One Go binary, Postgres, River for jobs. The console in `web/` is Vite + React with Meta's Astryx components, served from a Cloudflare Worker. Deploying both: `docs/deploy.md`. Backups and restoring: `docs/backups.md`.

## Product direction and implementation status

Go owns conversations, access control, policy, approvals, jobs and provider execution. Vite/React remains the web client, including phone approvals. A future Rust/GPUI native client will use the same HTTP API; WebAssembly is not required.

Channels and providers are separate concepts: email is a channel; a connected account supplies inbound and/or outbound capabilities. The hosted deployment currently receives through Cloudflare and sends through SES. Resend is available for self-hosted installations, including sending, domain verification, inbound email and delivery webhooks. See the Resend setup in `docs/deploy.md`. Consolidating an existing Gmail or Microsoft 365 mailbox will require a separate mailbox integration.

The first foundation changes separate outbound transport (`sending.Sender`) from domain management (`sending.DomainProvider`) and isolate the SES adapter. The configured adapter selects SES or Resend; domain verification stores provider-specific DNS records, including MX priorities. Inbound addresses use the configured receiving domain. Approval and delivery semantics remain in the core rather than in a client or provider adapter.

Verified SES, Resend and SignalWire delivery notifications are stored in `provider_events` before their effects are applied. Events that precede the send record remain pending and are reconciled after dispatch or by the minute sweep. Receipt processing and its resulting action state, audit event and opt-out are transactional. Unmatched receipts are retained for later matching; there is currently no automatic retention policy. Matching requires a recorded provider message ID: an interrupted send with no recorded ID still needs human investigation. SES sends disable SDK retries, and saving their outcome uses a bounded context independent of the caller's cancellation.

Next implementation slices:

1. Expand instance-level provider configuration into per-account connections where needed.
2. Harden the optional Docker web deployment for production operations.
3. Daily inbox workflow: assignment, internal notes, search, attachment handling, unread state and live updates.
4. An existing-mailbox integration selected with the first users.
5. Agent assistance and business-system integrations, followed by the native client against the shared API.

The core should remain independently runnable within Visioffice. Business-specific records belong behind an integration boundary (Links).

## Run it

Needs Go 1.27 and a local Postgres 17.

```bash
make test      # creates and migrates visicomms_test on first run
make dev       # creates visicomms_development, migrates, serves on :8080
curl localhost:8080/up
```

Or the API and database in Docker: `docker compose up` (Postgres on host port 5433, app on 8080). For the complete Resend setup with the React console, copy `.env.example` to `.env`, fill in your settings, then run `docker compose --profile web up --build`; the console is on port 8081. See `docs/deploy.md` before exposing an installation publicly.

The console:

```bash
cd web && npm install && npm run dev   # http://localhost:5173, /api forwarded to :8080
./bin/visicomms seed demo              # a demo workspace with the mockups' cases
./bin/visicomms invite <workspace id> you@example.com "Your Name"
```

Open the invite link and create a passkey. People sign in with passkeys only.

## Commands

| Command | Does |
| --- | --- |
| `visicomms migrate` | Applies River's and the app's migrations |
| `visicomms serve [--migrate]` | HTTP API and job workers; refuses to start with pending migrations unless `--migrate` applies them first |
| `visicomms workspace create <name> [timezone]` | Creates an empty workspace (time zone defaults to America/Los_Angeles) |
| `visicomms invite <workspace> <email> <name>` | Prints a one-time invite link (7 days) |
| `visicomms agent create <workspace> <name>` | Makes an agent and prints its token once (only its hash is stored). New agents are in approve mode |
| `visicomms agent list <workspace>` | Lists agents, their mode, and whether they are revoked |
| `visicomms agent revoke <workspace> <agent>` | Stops an agent's token at once |
| `visicomms seed demo [name]` | Creates a demo workspace with a sandbox sender |
| `visicomms fax connect <workspace> <number>` | Assigns an existing fax-capable SignalWire number after checking provider ownership; does not purchase or reconfigure it |
| `visicomms signalwire connect <workspace> <space> <project>` | Connects a workspace's own SignalWire project; token and signing key come from `SIGNALWIRE_CONNECT_API_TOKEN` and `SIGNALWIRE_CONNECT_SIGNING_KEY` and are stored encrypted |
| `visicomms signalwire brands` | Hosted: lists the 10DLC brands and campaigns registered through the parent project, read live |
| `visicomms signalwire registry <workspace> <brand> [campaign...]` | Hosted: records which brand and campaigns a workspace's project sends under, after checking them with the parent |
| `visicomms sms connect <workspace> <number>` | Assigns an existing SMS-capable SignalWire number after checking provider ownership; does not purchase or reconfigure it |
| `visicomms version` | Prints the build SHA |

## Environment

| Variable | Default |
| --- | --- |
| `VISICOMMS_ENV` | `development` (`production` in the Docker image) |
| `DATABASE_URL` | `postgres://localhost:5432/visicomms_development` in development; required elsewhere |
| `PORT` | `8080` |
| `INBOUND_EMAIL_SECRET` | none: without it every inbound email is rejected |
| `APP_URL` | `http://localhost:5173`; invite links point here, and https makes the session cookie Secure |
| `WEBAUTHN_RP_ID` | `localhost`; the domain passkeys belong to |
| `PUBLIC_URL` | `SIGNALWIRE_PUBLIC_URL`; the API's public address, where sites load the chat widget (`/chat/widget.js`) |
| `EMAIL_PROVIDER` | `ses`, or `resend` when only a Resend key is present; set explicitly when both keys exist |
| `RESEND_API_KEY` | none; required for Resend sending, domain setup and received-email retrieval |
| `RESEND_WEBHOOK_SECRET` | none; the Resend endpoint's `whsec_…` signing secret; required when `EMAIL_PROVIDER=resend` |
| `INBOUND_EMAIL_DOMAIN` | `in.visicomms.com` for SES; must be set to your receiving domain for Resend |
| `AWS_SES_ACCESS_KEY_ID`, `AWS_SES_SECRET_ACCESS_KEY` | none: without them, sends through SES stay queued |
| `AWS_SES_REGION` | `us-west-2` |
| `WEBAUTHN_ORIGINS` | `APP_URL`; comma-separated page origins allowed to use passkeys |
| `SIGNALWIRE_SPACE_URL` | none; your `name.signalwire.com` hostname |
| `SIGNALWIRE_PROJECT_ID`, `SIGNALWIRE_API_TOKEN` | none; installation-level project and token for SMS |
| `SIGNALWIRE_SIGNING_KEY` | none; the same project's webhook signing key, separate from its API token |
| `SIGNALWIRE_PUBLIC_URL` | none; the canonical public HTTPS origin of the Go webhook routes; set alone (with `VISICOMMS_MASTER_KEY`) when every workspace connects its own project |
| `SIGNALWIRE_PARENT_SPACE_URL`, `SIGNALWIRE_PARENT_PROJECT_ID`, `SIGNALWIRE_PARENT_API_TOKEN` | none; hosted only, for the `signalwire brands` and `registry` operator commands |
| `VISICOMMS_MASTER_KEY` | none; 32 bytes as hex or base64; seals workspace connector credentials. Without it, workspaces can only use the installation's SignalWire project |
| `TEST_DATABASE_URL` | `postgres://localhost:5432/visicomms_test`; each test package appends its name |

## Layout

| Path | What |
| --- | --- |
| `cmd/visicomms` | The binary |
| `internal/db` | Pool, migration runner, embedded SQL migrations |
| `internal/jobs` | River client; workers register in `register` |
| `internal/web` | HTTP API, including `POST /webhooks/email` |
| `internal/email` | Parses and triages inbound email (bounces, auto-replies, opt-outs) |
| `internal/untrusted` | Fences contact text; flags attempts to instruct an agent |
| `internal/ingest` | Stores inbound email/SMS as contacts, conversations and events, including opt-outs and dedupe |
| `internal/phone` | International phone-number normalization |
| `internal/auth` | Passkey sign-in: invites, WebAuthn ceremonies, sessions |
| `internal/sending` | SES/Resend adapters, domain verification, delivery events, the From address, and the action executor |
| `internal/agent` | Agent tokens, and what an agent can read and do: the inbox with contact text fenced, drafts that wait for a person, resolve, escalate |
| `internal/mcp` | The agent API as MCP tools at `/mcp` |
| `internal/review` | The console's queries and decisions: needs attention, threads, approve/reject/take over, approved info |
| `internal/demo` | The demo workspace seed |
| `web/` | The console (Vite, React, Astryx) and its Cloudflare Worker |
| `inbound/` | The Email Worker for `in.visicomms.com`: stores each raw message in R2, posts it signed to `/webhooks/email`, retries through a queue |
| `internal/config` | Settings from the environment |
| `internal/testutil` | Migrated test database |

## Agent API

Outside agents (Claude, a script, a custom loop) use `/api/v1` with `Authorization: Bearer <agent token>`. A token belongs to one agent in one workspace; a person can also make and revoke them from the console API (`/api/agents`).

| Route | Does |
| --- | --- |
| `GET /api/v1/me` | The agent and its workspace |
| `GET /api/v1/threads?waiting=true&state=&limit=` | The inbox, most recently active first; `waiting` keeps threads where the customer is waiting for a reply |
| `GET /api/v1/threads/{id}?all=true` | One thread: contact, the last 20 events (or all), pending drafts, linked records |
| `POST /api/v1/threads/{id}/drafts` | `{subject?, body, reason}`: a reply for a person to approve. Returns `held` with the rule, or `denied` (opted out, no address) |
| `POST /api/v1/threads/{id}/escalate` | `{note}`: puts the thread on the needs-attention list |
| `POST /api/v1/threads/{id}/resolve` | `{note?}`: closes the thread; a new message reopens it |
| `GET /api/v1/proposals?status=` and `/{id}` | The agent's own drafts, the person's decision note, and the send's state |
| `GET /api/v1/contacts?q=` | Contacts by name, email or phone |
| `GET /api/v1/events?after=<cursor>` | Everything since a cursor, oldest first, with `next_after` for the next call |

Anything a contact wrote comes back inside `<contact_message>` tags, with `suspected_injection` when it looks like an attempt to instruct an agent. Nothing an agent drafts is sent until a person approves it; a person's edit replaces the words and keeps the recipients and threading.

### MCP

The same tools over the Model Context Protocol at `POST /mcp` (streamable HTTP, stateless), with the same agent token: `whoami`, `threads_list`, `threads_get`, `threads_draft`, `threads_escalate`, `threads_resolve`, `proposals_list`, `proposals_get`, `contacts_search`, `events_since`. For Claude Code:

```bash
claude mcp add --transport http visicomms https://api.visicomms.com/mcp \
  --header "Authorization: Bearer $VISICOMMS_AGENT_TOKEN"
```

Chat clients that only connect with OAuth (Claude.ai, ChatGPT) are not supported yet.

## Rules the schema enforces

- `events` is append-only: UPDATE and DELETE raise. Corrections are new events with `corrects_event_id`.
- An `approved_info_versions` row is frozen once approved, and only a human principal can approve it.
- Inbound events dedupe on `(workspace_id, provider, provider_ref)`.

## Inbound email

The Cloudflare email worker (`inbound/`) posts to `POST /webhooks/email` signed with `X-VisiComms-Signature` (hex HMAC-SHA256 of `<timestamp>.<body>`) and `X-VisiComms-Timestamp`. Requests more than 5 minutes old are refused. The `to` address selects the workspace through `inbound_addresses`.

Each email is stored in one transaction: the contact (unverified, since From can be forged), the thread (matched by References to mail we received or sent, else an open thread with the same contact from the last 24 hours, else new), and an untrusted `email.received` event. An unsubscribe request sets the email opt-out right there, with no agent involved. Bounces, auto-replies and no-reply senders are classified so they never wake an agent.


## SMS

SignalWire provides the initial SMS transport. The Connections page shows assigned numbers and the Compatibility webhook URL. The instance operator assigns existing SMS-capable numbers with `sms connect`; workspace users cannot claim unassigned numbers from a shared hosted project. Replies remain on the original local/remote number pair, use the shared executor, and recheck approval, pause, take-over, routing and opt-out state before sending. Provider timeouts remain unknown and are not retried automatically.

Incoming STOP keywords block SMS and cancel queued SMS actions across the contact's conversations. START/UNSTOP clear a keyword STOP but never remove a manual block. Email opt-outs stay independent. Phone identities are not automatically merged with email contacts. This first slice supports inbound conversations and replies, not initiating texts to a new number. See `docs/deploy.md` for configuration and current limits.

SignalWire also provides two-way PDF fax. Connections includes a PDF uploader and new-fax composer; fax conversations support replies, document downloads, page counts and delivery/failure tracking. Documents are immutable, scoped to a workspace, and stored in Postgres (10 MB maximum per PDF). Outbound transmissions use temporary document links; incoming PDFs are archived after a verified callback. See [fax setup](docs/deploy.md#signalwire-fax) for number routing. This integration has local automated coverage; live carrier verification is still required.

## License

Copyright (C) 2026 RegAtlas, LLC. AGPL-3.0-only, matching VisiGrid and VisiConvert. See [LICENSE](LICENSE). Third-party components keep their own licenses.
