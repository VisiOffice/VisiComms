# Deploying VisiComms

Two pieces, deployed separately:

| Piece | Where | How |
| --- | --- | --- |
| API (Go binary + Postgres) | One Hetzner server, `api.<domain>` | Kamal (`config/deploy.yml`) |
| Console (Vite build + Worker) | Cloudflare Workers, `app.<domain>` | Wrangler (`web/wrangler.jsonc`) |

The Worker serves the console and forwards `/api/*` to the API, so the browser sees one origin. Inbound email webhooks from the email worker go straight to `api.<domain>/webhooks/email`.

## One-time setup

1. **Server.** Any small x86 server works, for example on Hetzner. CX types are EU only; in the US use CPX:

   ```bash
   hcloud server create --name visicomms-1 --type cpx22 --image ubuntu-24.04 --location hil --ssh-key <your key>
   ```

   Kamal installs Docker on first setup.

2. **DNS (Cloudflare).** `api.<domain>` → an A record to the server's IP, **DNS only (grey cloud)**. kamal-proxy gets its certificate from Let's Encrypt over HTTP, which the orange-cloud proxy would intercept. `app.<domain>` is set up by Wrangler as a Worker custom domain (step 5).

3. **Secrets.** Copy `.kamal/secrets.example` to `.kamal/secrets` (ignored by git) and fill it from your password manager. It needs:
   - `POSTGRES_PASSWORD`: a long random string (hex, so it needs no escaping in `DATABASE_URL`)
   - `INBOUND_EMAIL_SECRET`: a long random string; the email worker signs with the same value (its `WEBHOOK_SECRET`)

   - `VISICOMMS_MASTER_KEY`: 64 random hex characters (`openssl rand -hex 32`). It seals each workspace's connector credentials; losing it means every workspace re-enters them, so never rotate it by overwriting the field
   - `AWS_SES_ACCESS_KEY_ID`, `AWS_SES_SECRET_ACCESS_KEY`, `B2_BACKUP_KEY_ID`, `B2_BACKUP_APPLICATION_KEY`, and `BACKUP_HEARTBEAT_URL` (which may be blank)

   - `KAMAL_REGISTRY_PASSWORD`: a token that can push to your container registry (`write:packages` on ghcr.io)
   - `SES_EVENTS_TOPIC_ARN`: the SNS topic for SES delivery events (see Amazon SES below)

   Set `image` and `registry.username` in `config/deploy.yml` to your own registry. The example reads every value from 1Password with `op read`; any command that prints the value works. Make a missing secret stop the deploy rather than ship a blank one.

4. **API.**

   ```bash
   export VISICOMMS_HOST=<server ip> VISICOMMS_DOMAIN=<domain>
   kamal setup        # installs Docker, starts Postgres, builds, migrates, serves
   curl https://api.$VISICOMMS_DOMAIN/up
   ```

5. **Console.** In `web/wrangler.jsonc`, set `API_ORIGIN` to `https://api.<domain>` and uncomment the route with `app.<domain>`. Then:

   ```bash
   cd web && npm ci && npm run deploy
   ```

6. **First workspace and your invite.** The image's entrypoint is the binary, so `kamal app exec` takes its arguments. A real, empty workspace:

   ```bash
   kamal app exec 'workspace create "Your Business" America/Los_Angeles'
   kamal app exec 'invite <workspace id from the line above> you@example.com "Your Name"'
   ```

   Or `kamal app exec 'seed demo'` for a demo workspace with sample threads that sends nothing.

   Open the printed `https://app.<domain>/invite/...` link and create your passkey.

## Every deploy

```bash
kamal deploy                 # API: build, push, migrate under a lock, swap with no downtime
cd web && npm run deploy     # console
```

`serve --migrate` migrates before serving. If a migration fails, the new container never reports healthy and the old one keeps serving.

## Passkeys and domains

Passkeys are bound to `WEBAUTHN_RP_ID` (the bare domain) and work only on pages from `WEBAUTHN_ORIGINS` (`https://app.<domain>`). Both are set from `VISICOMMS_DOMAIN` in `config/deploy.yml`. Changing the domain later makes existing passkeys unusable, so pick the domain before inviting real users.

## Locally

```bash
make dev                         # API on :8080 (local Postgres)
cd web && npm install && npm run dev   # console on :5173, /api forwarded to :8080
./bin/visicomms seed demo
./bin/visicomms invite <workspace id> you@example.com "Your Name"
```

Passkeys work on `http://localhost` without HTTPS.

## Inbound email

Customer mail is forwarded to `in+<workspace>-<id>@in.visicomms.com`. Cloudflare Email Routing handles only the **`in.visicomms.com` subdomain**. visicomms.com's own mail stays with Google Workspace, and its MX and SPF records are not touched.

| Piece | Setting |
| --- | --- |
| Routing | Enabled for the name `in.visicomms.com` (MX `route{1,2,3}.mx.cloudflare.net` and SPF on the subdomain only), with subaddressing on |
| Rule | `in@in.visicomms.com` → Worker `visicomms-inbound`. With subaddressing it matches every `in+…` address; catch-all rules don't exist for subdomains |
| Worker | `inbound/` (`npm run deploy`). Secret `WEBHOOK_SECRET` = the API's `INBOUND_EMAIL_SECRET` |
| Storage | R2 bucket `visicomms-mail`: `raw/` (every message, stored first), `attachments/` (by SHA-256), `payloads/` (what was posted, for retries) |
| Retries | Queue `visicomms-inbound-retry` (backoff up to an hour, 20 tries), then `visicomms-inbound-dead` |

An address no workspace owns is bounced to the sender. Wrangler's `email routing` commands only accept zones, so subdomain routing and subaddressing were set through the Cloudflare API (`POST /zones/{zone}/email/routing/dns` with `name`, and `PATCH /zones/{zone}/email/routing` with `support_subaddress`).


## Self-hosting with Resend

Resend can replace both SES sending and the Cloudflare inbound email worker. No AWS or Cloudflare account is needed for this path. One installation-level Resend key serves its workspaces; each workspace must still prove ownership of its sending domain. Per-workspace provider keys are not yet supported.

1. Create a Resend API key with full access for domain management and received-email retrieval. A sending-only key is insufficient.
2. Enable receiving on a domain or subdomain in Resend and publish the receiving MX records it supplies. A dedicated subdomain such as `in.example.com` lets an existing Google Workspace or Microsoft 365 inbox keep its own MX records. Forward customer inquiries to the address VisiComms creates there.
3. Create a webhook in Resend targeting `https://comms.example.com/webhooks/resend`. Subscribe to `email.received`, `email.sent`, `email.delivered`, `email.delivery_delayed`, `email.bounced`, `email.complained`, `email.failed`, and `email.suppressed`. Copy its signing secret.
4. Copy `.env.example` to `.env` and set `EMAIL_PROVIDER=resend`, `RESEND_API_KEY`, `RESEND_WEBHOOK_SECRET`, and `INBOUND_EMAIL_DOMAIN`. For production set `APP_URL=https://comms.example.com`, `WEBAUTHN_RP_ID=comms.example.com`, and `WEBAUTHN_ORIGINS=https://comms.example.com`. The API must be reachable by Resend over HTTPS; a localhost-only deployment cannot receive its webhooks.
5. Run `docker compose --profile web up --build -d`. This adds an Nginx service on port 8081 that serves the Vite build and proxies `/api/` and `/webhooks/` to Go. For local use the sample origin is `http://localhost:8081`. Put the public HTTPS reverse proxy in front of this service for production.
6. Create a workspace and invite through the binary:

   ```bash
   docker compose exec app /visicomms workspace create "My Business" UTC
   docker compose exec app /visicomms invite <workspace-id> you@example.com "Your Name"
   ```

7. Open the invite at your configured origin and register a passkey. In Connections, add your sending domain, publish the DNS records shown (including the workspace ownership TXT record), and click **Check now**. Set your From address after verification. In the setup checklist, create the forwarding address on your configured inbound domain.

The Compose database password defaults to a local-development value; set `POSTGRES_PASSWORD` to a strong URL-safe password for a new production installation. Changing the variable does not change an existing database user's password. Configure backups and restrict exposed database/API ports for your deployment. Existing hosted Kamal/SES deployments continue using their current configuration.

Test the full loop with an external email: forward it into the generated inbox, reply from VisiComms, confirm receipt, and check the delivery event. API tests use a mock Resend service; they do not replace checking your account's DNS, receiving setup and webhook subscriptions.

Resend requests are not automatically retried after ambiguous send errors. Each send includes an action-based idempotency key, but unknown sends still require investigation. Signed delivery events are stored before application and deduped by provider and webhook ID; early events are replayed after the send is recorded. Incoming message bodies are retrieved from Resend, then stored through the same contact/thread, opt-out and untrusted-content pipeline as Cloudflare email. Retrieval or database failures return a server error so Resend can retry the webhook. Mail with no configured recipient is acknowledged without creating a thread.

Attachments currently retain metadata and a Resend locator, rather than being copied into local object storage; attachment downloads and independent archival are not implemented. Provider receipts currently have no automatic retention policy. Inbound domain DNS and the Resend webhook subscription are configured in Resend, not automatically provisioned by VisiComms.

API references: [sending](https://resend.com/docs/api-reference/emails/send-email), [received email](https://resend.com/docs/api-reference/emails/retrieve-received-email), [domain verification](https://resend.com/docs/api-reference/domains/create-domain), and [webhook verification](https://resend.com/docs/webhooks/verify-webhooks-requests).


## SignalWire SMS

SMS uses SignalWire's **Compatibility API**. It is independent of the chosen email provider. This version connects an existing SMS-capable number; it does not buy numbers, port them, or submit brand/campaign registrations.

There are two ways a workspace gets a SignalWire project:

- **The installation's project** (`SIGNALWIRE_*` below). Every workspace without a project of its own uses it. This is the usual single-business self-host.
- **The workspace's own project**, connected with `visicomms signalwire connect`. Its API token and signing key are sealed with the workspace's data key, which is sealed with `VISICOMMS_MASTER_KEY`. The hosted service runs this way, with no installation project.

Callback URLs are the same for every project. Each callback is checked against the project named in its `AccountSid`, using that project's signing key, and the number must be connected through that project. An unknown project or a bad signature gets a 403.

For the installation's project, set these values on the Go service (Docker Compose passes them through from `.env`):

```dotenv
SIGNALWIRE_SPACE_URL=yourspace.signalwire.com
SIGNALWIRE_PROJECT_ID=your-project-id
SIGNALWIRE_API_TOKEN=your-api-token
SIGNALWIRE_SIGNING_KEY=your-project-signing-key
SIGNALWIRE_PUBLIC_URL=https://comms.example.com
```

The public URL is an HTTPS **origin**, without a path or query. For the hosted split deployment it can be the API origin, such as `https://api.visicomms.com`; for the optional Nginx web service it can be the console's public origin. Use the signing key belonging to the project that owns the number. Signatures are enforced immediately using the exact configured origin and form parameters; proxy Host/forwarded headers cannot change the verification origin. The API token needs messaging and number-read access. No production credentials are bundled.

To give a workspace its own project instead, set `SIGNALWIRE_PUBLIC_URL` and `VISICOMMS_MASTER_KEY` (`openssl rand -hex 32`) on the service, then pass the token and signing key through the environment so they stay out of shell history:

```bash
SIGNALWIRE_CONNECT_API_TOKEN=… SIGNALWIRE_CONNECT_SIGNING_KEY=… \
  visicomms signalwire connect <workspace-id> yourspace.signalwire.com <project-id>
```

The command checks the credentials against SignalWire before storing them. Running it again for the same project replaces the token and signing key. A project belongs to one workspace, and a workspace cannot switch projects while numbers are still connected through the old one.

After migrating and restarting the service, assign the number to its workspace:

```bash
docker compose exec app /visicomms sms connect <workspace-id> +14155550123
```

The CLI checks that the number belongs to the workspace's project (its own, or the installation's) and supports SMS. It cannot reassign another workspace's number. Open Connections to see the assigned number. In SignalWire, configure the number's **Compatibility SMS URL**, method **POST**, to:

```text
https://comms.example.com/webhooks/signalwire/sms
```

Use the same hostname as `SIGNALWIRE_PUBLIC_URL`. This is a form-encoded Compatibility callback endpoint, not a JSON SWML endpoint. Outbound messages automatically specify `https://comms.example.com/webhooks/signalwire/sms/status` as their status callback. Neither number assignment nor startup changes remote number routing; set the webhook on the number in SignalWire yourself.

Send an external SMS to the number. It should appear in Needs attention. Open the thread and reply; the composer shows the original sending number and omits email subjects. The provider's API acceptance is distinct from its carrier delivery callback. Test STOP with a queued reply, then START; STOP must block new replies and cancel queued SMS. Replaying the same inbound MessageSid does not apply it again.

The first version sends text only, with a 1600-character limit. Incoming media is flagged by count but is not downloaded or archived. It records HELP/INFO as control messages but does not automatically reply to keywords or send STOP/START confirmation texts. Phone identities remain separate from email identities until an explicit linking workflow is built. The per-contact SMS block applies across local numbers in that workspace; START/UNSTOP can only clear a block whose source was an inbound STOP, never an operator block. It does not establish consent for new outbound campaigns.

Delivery notifications are deduped by message SID and status, stored durably, and replayed if they arrive before the send record. Terminal SMS states do not regress on late intermediate callbacks. Callback delivery is best-effort: there is currently no provider-status polling or manual reconciliation UI, so an accepted message can remain accepted if its callback never arrives. Unknown sends must be investigated before sending another copy. Numbers removed, transferred, or reconfigured at SignalWire need operator attention; this slice does not automatically resync number ownership. Voice remains unimplemented.

References: [Compatibility API webhooks](https://signalwire.com/docs/compatibility-api/guides/webhooks), [webhook security](https://signalwire.com/docs/compatibility-api/guides/webhook-security), and [incoming-number capabilities](https://signalwire.com/docs/server-sdks/reference/typescript/rest/compat/phone-numbers/list).

## SignalWire fax

Fax uses the same project and public origin as SMS, through the Compatibility API. Give the API token the necessary fax, number lookup and media access permissions. Assign an existing fax-capable number:

```sh
visicomms fax connect <workspace> +14155550123
```

The command verifies ownership and fax capability and assigns the number exclusively to that workspace. It does not purchase the number or change routing at SignalWire. In SignalWire, set the number's **Accept Incoming Calls As** to **Fax**, then configure **Handle Faxes Using** the Compatibility/LaML webhook, method **POST**:

```text
https://comms.example.com/webhooks/signalwire/fax
```

Use a dedicated fax number if the number is also needed for voice: the incoming-call setting controls how calls are answered. The app returns `<Receive>` instructions pointing to `/webhooks/signalwire/fax/received`. Outbound faxes set `/webhooks/signalwire/fax/status` automatically. All three endpoints verify signatures against `SIGNALWIRE_PUBLIC_URL` with the signing key of the project in `AccountSid`. Forward `/webhooks/`, `/api/` and `/fax-media/` to the Go server; the included nginx configuration does this. The public HTTPS origin must be reachable by SignalWire for PDF downloads.

Open **Connections → Fax**, select a PDF (maximum 10 MB), enter the destination in E.164 format, and send. Replies within a fax thread retain the original local and remote number pair. The explicit human send queues the exact document hash; workers recheck routing, workspace pause, take-over and opt-out before dispatch. Browser retries reuse a request ID to avoid queuing another copy. Ambiguous provider responses remain unknown and are never automatically retransmitted.

PDFs are stored immutably in Postgres, including received documents, so database backups contain the fax archive. Plan database capacity accordingly; there is currently no automatic document retention or deletion. Authenticated downloads are scoped to the user's workspace. SignalWire gets an unguessable bearer link valid for at most 24 hours and revoked when the send reaches a terminal state. Keep these links out of proxy access logs. Incoming media downloads allow only the configured SignalWire space or `files.signalwire.com`, without redirects. PDF checks cover file size and header, not a full PDF parser or malware scan.

Callbacks are durable and deduplicated, including delivery callbacks arriving before the send result. Received callbacks are acknowledged without downloading another copy once archived. Failed sends and receptions appear in Needs attention. Delivery depends on callbacks; there is no provider polling. Live verification still requires a configured number: send a short PDF to a known fax endpoint, verify delivered status and page count, then fax back and confirm the archived PDF opens. Automated tests use a fake carrier and do not send paid transmissions.

Reference: [SignalWire send-fax example](https://signalwire.com/developers/demos/r/send-a-fax.html) and [inbound fax callbacks](https://signalwire.com/docs/compatibility-api/guides/callback-for-inbound-fax).

## Postal mail (Lob)

Letters are printed and mailed by each workspace's own [Lob](https://www.lob.com) account. The server needs only `VISICOMMS_MASTER_KEY` (to seal the Lob key) and `PUBLIC_URL` (where Lob sends webhooks; it defaults to `SIGNALWIRE_PUBLIC_URL`). There is no installation-wide Lob key: a workspace without its own key cannot send letters.

In the console, **Connections → Mail**:

1. Paste Lob's **secret** API key. A `test_` key renders letters without mailing them, and every such letter is marked "Test, not mailed". A `live_` key prints and mails, and Lob bills the account.
2. Enter the return address. With a live key, Lob checks it with USPS.
3. Set the monthly limit. Letters that would pass it are refused, counted from VisiComms' estimate of each letter's cost (Lob's API does not return prices).
4. In Lob, **Webhooks**, add a webhook for the letter events pointing at the URL shown, then paste its secret back under **Change**. Without it letters still send but show no tracking.

A person adds a contact's mailing address from any of the contact's threads (Lob standardises it and says whether USPS can deliver there), then sends a PDF from **Send a letter**. Letters are always a person's explicit send; agents cannot send or propose them yet. The executor re-checks the PDF hash, address, return address, opt-out and monthly limit before calling Lob, sends the PDF itself (never a link), and uses the action ID as Lob's idempotency key, so a retry after a timeout cannot print a second copy. Lob adds a blank first page for the address (`insert_blank_page`), so any US letter–size PDF works.

Webhooks at `/webhooks/lob/{connector}` are checked with that account's secret (HMAC-SHA256 of the timestamp, `.`, and the body) and only match letters sent through the same connector. Tracking scans, delivery, returns and failures land on the contact's postal thread; returned and failed letters appear in Needs attention, and an address a letter came back from takes no more letters until it is saved again. A letter can be cancelled from its thread until Lob's send date. Lob's proof link expires after 30 days.

## SignalWire voice

Each connected number has a call flow, set on the console's **Phone** page or by an agent (`call_flows_list` and `call_flow_set` over MCP, `PUT /api/v1/numbers/{number}/call-flow`). An agent in approve mode proposes a change that waits for a person to activate it; an autonomous agent's change applies at once; an observer cannot change flows. A number with no flow takes a message.

The answer modes are **ring in the app** (below), **forward** to a phone number, an **IVR** menu whose keys can do any of the others, **AI answers** (below), and **voicemail**. A ring or forward can have a next step for when nobody answers, up to four steps, e.g. ring the app for 20 seconds, then forward to a mobile, then the AI. A call that runs out of steps ends in voicemail. Voicemails are transcribed by SignalWire.

Point a number's Compatibility voice webhook (POST) and its status callback at:

```text
https://comms.example.com/webhooks/signalwire/voice
https://comms.example.com/webhooks/signalwire/voice/status
```

Callbacks are verified with the signing key of the project in `AccountSid`, like SMS and fax. Each call is recorded on the caller's voice thread (`call.received`, then `call.answered`, `call.missed`, `call.failed` or `voice.voicemail`, plus `voice.transcript`). A missed call or voicemail leaves the caller waiting in Needs attention; an answered call counts as a reply. Voicemail audio is moved into the recordings bucket like call recordings (below); the event keeps SignalWire's URL.

### Ring in the app

The console's sidebar has a phone. With **Take calls here** on, the browser registers at the workspace's SignalWire project with a token from `POST /api/phone/token` (SignalWire's Relay browser token, as `visicomms-<person id>`) and checks in every 30 seconds. A ring-in-the-app step dials every browser that checked in within 75 seconds, as `<Verto>` addresses in one `<Dial>`; the first to answer takes the call, and the caller's thread opens. If nobody is online the step is skipped at once, so the caller goes straight to the next step rather than ringing into nothing. The phone rings only while a console tab is open, which is why it usually has a forward after it.

### AI answering

The AI answers through an OpenAI project over SIP: SignalWire dials `sip:<project>@sip.api.openai.com` with the call's ID in an `X-VisiComms-Call` header, OpenAI announces the call to a webhook, and VisiComms accepts it and follows the conversation over a websocket. The audio never passes through VisiComms; SIP negotiates its format, so the session never sets one. It needs `OPENAI_API_KEY`, `OPENAI_PROJECT_ID` and `OPENAI_WEBHOOK_SECRET`; without all three the mode is not selectable.

`OPENAI_VOICE_API` picks which of OpenAI's voice APIs answers:

- `live`, GPT-Live (`gpt-live-1`). It speaks and listens; tasks (taking a message, transferring, hanging up) go to a backend model (`gpt-6-luna`) that runs the tools. The project must have GPT-Live SIP enabled.
- `realtime`, the default: `gpt-realtime-2.1` speaks and runs the tools itself.

In the OpenAI project's settings, point a webhook at the URL below and subscribe it to the matching event: `live.transport.incoming` for `live`, `realtime.call.incoming` for `realtime`. Subscribing both is harmless, and makes switching a deploy rather than a settings change: OpenAI can announce one call through both, and VisiComms answers only through the API it is set to.

```text
https://comms.example.com/webhooks/openai/realtime
```

Whichever service receives the webhook turns away calls it did not dial. If another system shares the OpenAI project, moving the webhook here is part of the cutover: from then on, every number still answered by the other system's AI goes to voicemail until it moves too.

Each AI flow has an opening line, the business's rules for calls, a voice (GPT-Live speaks more voices than the realtime model, which falls back to marin), and optionally a number to transfer urgent callers to. What the AI knows is the workspace's latest **approved** info, never anything a person has not approved; `{"articles": [{"title", "content", "channels"}]}` is read as titled sections, and an article whose channels leave out `phone` is skipped. Draft a version from a file with `visicomms approved-info draft <workspace> <file.json>` and approve it under **Rules**.

If the AI has not started speaking eight seconds after it picks up, or OpenAI never answers, the caller is sent to voicemail. When the call ends, a `call.ai_handled` event carries the transcript (untrusted, like any message) and a summary. A message the AI took leaves the caller waiting in Needs attention and notifies like a voicemail ("Calls the AI answered").

Every call the AI answers is recorded, both sides on separate channels, and the AI opens with "This call may be recorded." before the opening line (California requires every party to know). When the recording is ready a `call.recorded` event with its SignalWire URL lands on the thread. The console plays recordings and voicemails through `GET /api/calls/{id}/recording`, so the provider link and credentials never reach the browser and only people in the workspace can listen.

**Recordings storage.** Every minute a sweep (`internal/recordings`) copies each new recording and voicemail from SignalWire into the installation's bucket, records its size and SHA-256, and only then deletes it from SignalWire. A failed copy keeps SignalWire's and is retried with backoff (up to six hours apart); recordings made before the sweep existed are picked up the same way. Playback streams from the bucket once a recording is copied, and from SignalWire until then. **A year after the call both copies are deleted** and playback answers 410. Without `RECORDINGS_BUCKET`, recordings stay at SignalWire and are only deleted there after the year.

Fetching from SignalWire: an authenticated request for a recording answers 302 to `files.signalwire.com`; VisiComms follows that one redirect, there only, without the credentials.

Example setup (Backblaze B2, over its S3 API, like the backups):

1. A **private** bucket `visicomms-recordings` with **default encryption (SSE-B2)** on, no Object Lock, and the lifecycle rule **Keep only the last version of the file**. VisiComms deletes by version, so a deletion is final; the rule purges anything hidden by hand.
2. An application key limited to that bucket, with only the capabilities the sweep and playback use:
   ```bash
   b2 account authorize            # master application key
   b2 key create --bucket visicomms-recordings visicomms-recordings listBuckets,listFiles,readFiles,writeFiles,deleteFiles
   b2 account clear
   ```
   Store the keyID and applicationKey in your password manager and read them in `.kamal/secrets`; the deploy passes them as `RECORDINGS_S3_KEY_ID` and `RECORDINGS_S3_SECRET`; the bucket, endpoint and region are in `config/deploy.yml`.

### Entrances (door intercoms)

An entrance is a door or gate intercom, set up under **Phone → Entrances**. Its calls run the entrance's own flow instead of the dialled number's, so the AI answers as a gatekeeper: it asks who the visitor is and who they are here for, follows the entrance's visitor rules, never says the door is open or whether anyone is home, and takes a message. Nothing in VisiComms opens a door. Calls go on one thread under a contact named after the entrance.

A call is from an entrance in one of two ways, never from anything the visitor says:

- **Caller ID.** An on-site bridge calls one of the workspace's numbers from a fixed number; enter that number as the entrance's caller ID. Only calls from it to this workspace's numbers run the entrance's flow.
- **SIP account.** **Make a SIP account** creates a SignalWire SIP endpoint (`gate-…`) in the workspace's project, whose calls SignalWire hands to `/webhooks/signalwire/voice` and `/voice/status`. Enter its server (`<space>.sip.signalwire.com`), port 5060 UDP, user ID, password and recipient number in UniFi Access under **Interface Designer → the intercom → Doorbell Call → Third-Party SIP**, then add the recipient as a receiver. The password is shown once and never stored; replacing the account makes a new user ID and password and deletes the old endpoint. A SIP call from any account that is not an entrance's is hung up.


## Notifications

Each person chooses, per kind of event, whether they are told **right away** (email, text, or both), in a **daily summary** email at 8am in the workspace's time zone, or **not at all**, on the console's Notifications page. An agent can change them with `notifications_set` over MCP or `PUT /api/v1/notifications/{person}` (not in observe mode). The kinds are voicemails, calls the AI answered, missed and answered calls, texts, emails, faxes, drafts waiting for approval, escalations, and failed sends.

- **Email** needs `NOTIFY_FROM`, an address the email provider can send from (for SES, on a verified domain). Without it nothing is emailed and the page says so.
- **Texts** go from one of the workspace's own SMS numbers to the person's mobile, so that number must be on an approved 10DLC campaign.

Instant notifications wait 20 seconds so a voicemail's transcript goes with it. Each one is recorded before it is sent, so a restart can drop a notification but never repeats one.

## Multi-workspace: connecting a SignalWire subproject

An installation serving several businesses can run each workspace on its own subproject of one parent SignalWire space. Brands and campaigns are registered with SignalWire, so a subproject that moves from another system keeps them and nothing is refiled.

The parent project's API token (with the Management scope) can create subprojects and mint their API tokens, but SignalWire shows a subproject's **signing key** only when it makes one: on creation, and on rotation. Two commands use this, both run from your machine with the parent's credentials (`SIGNALWIRE_PARENT_*`), so production never carries them:

- **`signalwire provision <workspace-id>`** creates a subproject `visicomms-<workspace name>`, mints a token (messaging, fax, calling, numbers), and seals both with the signing key into the workspace's connector. Neither value is shown to anyone. Use it for a new workspace.
- **`signalwire adopt <workspace-id> <subproject-id>`** connects an existing subproject, such as one another system uses, with a newly minted token. Its signing key is either the current one, copied once from the subproject's **API** page in the dashboard into `SIGNALWIRE_CONNECT_SIGNING_KEY`, or, with `--rotate`, a new one. Rotating stops the old key within a minute or two, so do it only once nothing else verifies that subproject's callbacks. Re-running adopt mints a fresh token; it does not revoke the old one.

SignalWire signs each callback with the key of the subproject that owns the number, not the parent's.

1. **Tunnel to the production database.** It listens only on the server's localhost, so operator commands run from your machine and no secret goes through `kamal app exec` (whose command line lands in the audit log):

   ```bash
   ssh -N -L 15432:127.0.0.1:5432 root@<VISICOMMS_HOST> &
   ```

   The commands below use 1Password's `op run`, which resolves `op://` references into the environment; `\$POSTGRES_PASSWORD` is escaped so it expands inside it, never in your shell history. Point the references at your own items.

2. **Connect the project** to its workspace (`visicomms workspace create` first, if needed). A new workspace gets a new subproject:

   ```bash
   SIGNALWIRE_PARENT_SPACE_URL=… SIGNALWIRE_PARENT_PROJECT_ID=… SIGNALWIRE_PARENT_API_TOKEN=… \
   POSTGRES_PASSWORD=op://<vault>/<item>/POSTGRES_PASSWORD \
   VISICOMMS_MASTER_KEY=op://<vault>/<item>/VISICOMMS_MASTER_KEY \
   op run -- sh -c "DATABASE_URL=postgres://visicomms:\$POSTGRES_PASSWORD@localhost:15432/visicomms \
        SIGNALWIRE_PUBLIC_URL=https://api.<domain> visicomms signalwire provision <workspace-id>"
   ```

   A subproject moving from another system is adopted instead. While that system still handles its callbacks, pass the signing key from the dashboard (set `SIGNALWIRE_CONNECT_SIGNING_KEY=…` and run `signalwire adopt <workspace-id> <subproject-id>`). At cutover, once its numbers' webhooks point here, run `signalwire adopt <workspace-id> <subproject-id> --rotate` without it.

3. **Record its brand and campaign.** `visicomms signalwire brands` lists every brand and campaign registered through the parent, read live. Then attach the right one; the campaign must belong to the brand:

   ```bash
   SIGNALWIRE_PARENT_SPACE_URL=… SIGNALWIRE_PARENT_PROJECT_ID=… SIGNALWIRE_PARENT_API_TOKEN=… \
   POSTGRES_PASSWORD=op://<vault>/<item>/POSTGRES_PASSWORD \
   op run -- sh -c "DATABASE_URL=postgres://visicomms:\$POSTGRES_PASSWORD@localhost:15432/visicomms visicomms signalwire registry <workspace-id> <brand-id> <campaign-id>"
   ```

4. **Connect its numbers** with `sms connect` and `fax connect`, through the same tunnel with `VISICOMMS_MASTER_KEY` and `SIGNALWIRE_PUBLIC_URL` set. This only records them; their webhooks keep pointing wherever they point now until you change them in SignalWire, one number at a time.

The parent credentials are only needed on the operator's machine, so production does not carry them.
