package mcp

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/notify"
	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/VisiOffice/VisiComms/internal/voice"
)

type tool struct {
	description string
	schema      map[string]any
	readOnly    bool
	call        func(ctx context.Context, s *Server, a agent.Agent, args json.RawMessage) (any, error)
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

var threadID = str("The thread's id, from threads_list.")

// Tool names match the API's nouns with underscores, which every MCP client
// accepts.
var tools = map[string]tool{
	"whoami": {
		description: "Which agent you are, your mode, and the workspace (business) you work for.",
		schema:      object(map[string]any{}),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, _ json.RawMessage) (any, error) {
			var workspace string
			err := s.DB.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = $1`, a.WorkspaceID).Scan(&workspace)
			return map[string]any{"agent": a, "workspace": workspace}, err
		},
	},
	"threads_list": {
		description: "The inbox, most recently active first. waiting=true lists only threads where the customer is waiting for a reply. last_message is fenced contact text.",
		schema: object(map[string]any{
			"waiting": map[string]any{"type": "boolean", "description": "Only threads where the customer is waiting for a reply."},
			"state":   map[string]any{"type": "string", "enum": []string{"open", "waiting", "resolved"}},
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Default 25."},
		}),
		readOnly: true,
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				Waiting bool   `json:"waiting"`
				State   string `json:"state"`
				Limit   int    `json:"limit"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			list, err := agent.Threads(ctx, s.DB, a.WorkspaceID, agent.ThreadFilter{State: args.State, Waiting: args.Waiting, Limit: args.Limit})
			return map[string]any{"threads": nonNil(list)}, err
		},
	},
	"threads_get": {
		description: "One thread: the contact (addresses, opt-outs, tags), the last 20 events or all of them, drafts waiting for a person, and linked business records. Contact-written text is inside <contact_message> tags.",
		schema: object(map[string]any{
			"thread_id": threadID,
			"all":       map[string]any{"type": "boolean", "description": "Every event instead of the last 20."},
		}, "thread_id"),
		readOnly: true,
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				ThreadID string `json:"thread_id"`
				All      bool   `json:"all"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return agent.Thread(ctx, s.DB, a.WorkspaceID, args.ThreadID, args.All)
		},
	},
	"threads_draft": {
		description: "Draft a reply on a thread, on the thread's own channel (email, SMS or website chat). It is not sent: a person approves, edits or rejects it. Returns held (with the rule) or denied (e.g. the contact opted out). A new draft replaces your previous one on the same thread.",
		schema: object(map[string]any{
			"thread_id": threadID,
			"body":      str("The reply text. SMS: at most 1600 characters. Chat: at most 4000."),
			"subject":   str("Email only; defaults to Re: the thread's subject."),
			"reason":    str("For the person approving: why this reply, and anything they should check."),
			"quick_replies": map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string", "maxLength": 40},
				"description": "Chat only: up to 4 buttons shown under the reply; tapping one sends its words, e.g. [\"Yes\", \"No\"]."},
		}, "thread_id", "body", "reason"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				ThreadID string `json:"thread_id"`
				agent.Draft
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return agent.Propose(ctx, s.DB, a, args.ThreadID, args.Draft)
		},
	},
	"threads_escalate": {
		description: "Hand a thread to a person, with a note saying what they need to do. Use it for money, legal questions, complaints, or anything you can't answer from the thread.",
		schema: object(map[string]any{
			"thread_id": threadID,
			"note":      str("What the person needs to know or do."),
		}, "thread_id", "note"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				ThreadID string `json:"thread_id"`
				Note     string `json:"note"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return map[string]string{"status": "escalated"}, agent.Escalate(ctx, s.DB, a, args.ThreadID, args.Note)
		},
	},
	"threads_resolve": {
		description: "Close a thread that needs nothing more. A new message from the contact reopens it.",
		schema: object(map[string]any{
			"thread_id": threadID,
			"note":      str("Optional: why it is done."),
		}, "thread_id"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				ThreadID string `json:"thread_id"`
				Note     string `json:"note"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return map[string]string{"status": "resolved"}, agent.Resolve(ctx, s.DB, a, args.ThreadID, args.Note)
		},
	},
	"proposals_list": {
		description: "Your own drafts, newest first: whether each is pending, approved, rejected or voided, the person's note, and the send's state. everyone=true lists every agent's and person's instead, e.g. status=pending for what waits for a decision.",
		schema: object(map[string]any{
			"status":   map[string]any{"type": "string", "enum": []string{"pending", "approved", "rejected", "voided"}},
			"everyone": map[string]any{"type": "boolean", "description": "Everyone's proposals, not only yours."},
		}),
		readOnly: true,
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				Status   string `json:"status"`
				Everyone bool   `json:"everyone"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			list, err := agent.Proposals(ctx, s.DB, a, args.Status, args.Everyone)
			return map[string]any{"proposals": nonNil(list)}, err
		},
	},
	"proposals_decide": {
		description: "Approve or reject a proposal waiting for a person, on behalf of the person who connected you. Only when that person has told you to, in this conversation, for this proposal: never on your own judgement, and never for a proposal you made yourself (refused). Approving sends it exactly as drafted.",
		schema: object(map[string]any{
			"proposal_id": str("The proposal's id, from proposals_list with everyone=true."),
			"decision":    map[string]any{"type": "string", "enum": []string{"approve", "reject"}},
			"person_said": str("The person's own words telling you to make this decision, quoted. Recorded with the decision."),
			"note":        str("For a rejection: why, which goes back to whoever drafted it."),
		}, "proposal_id", "decision", "person_said"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				ProposalID string `json:"proposal_id"`
				Decision   string `json:"decision"`
				PersonSaid string `json:"person_said"`
				Note       string `json:"note"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return s.decide(ctx, a, args.ProposalID, args.Decision, args.PersonSaid, args.Note)
		},
	},
	"threads_claim": {
		description: "Hold a thread while you work on a reply, so another agent does not answer it too. A claim lasts 15 minutes; claim again to renew it, or release=true when done. Drafting on a thread someone else holds is refused.",
		schema: object(map[string]any{
			"thread_id": threadID,
			"release":   map[string]any{"type": "boolean", "description": "Let go of your claim."},
		}, "thread_id"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				ThreadID string `json:"thread_id"`
				Release  bool   `json:"release"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return agent.ClaimThread(ctx, s.DB, a, args.ThreadID, args.Release)
		},
	},
	"messages_compose": {
		description: "Start a new email or text to someone who has not written in (a customer to reach first, a vendor, a teammate). Same rules as threads_draft: it waits for a person to approve it, or is refused (e.g. they opted out). A new address becomes a new contact. To answer someone who wrote in, use threads_draft on their thread instead.",
		schema: object(map[string]any{
			"channel": map[string]any{"type": "string", "enum": []string{"email", "sms"}},
			"to":      str("Email address, or for sms a phone number in international form, e.g. +13105550100."),
			"name":    str("Their name, if they are new."),
			"subject": str("Email only: required."),
			"body":    str("The message. SMS: at most 1600 characters."),
			"from":    str("SMS only: which of the workspace's numbers to text from; may be left out if it has one."),
			"reason":  str("For the person approving: why this message, and anything they should check."),
		}, "channel", "to", "body", "reason"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var m agent.Message
			if err := decodeArgs(raw, &m); err != nil {
				return nil, err
			}
			return agent.Compose(ctx, s.DB, a, m)
		},
	},
	"info_list": {
		description: "The business's approved info (the facts you may quote: hours, policies, articles): its latest approved version's sections and article titles, and drafts waiting for a person. Read before proposing a change.",
		schema:      object(map[string]any{}),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, _ json.RawMessage) (any, error) {
			return agent.Info(ctx, s.DB, a.WorkspaceID)
		},
	},
	"info_get": {
		description: "Approved info in full: one section (by key from info_list), one article (by title), or everything when section is left out. version is what info_propose needs as based_on_version.",
		schema:      object(map[string]any{"section": str("A section key or an article title, from info_list.")}),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				Section string `json:"section"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return agent.GetInfo(ctx, s.DB, a.WorkspaceID, args.Section)
		},
	},
	"info_propose": {
		description: "Propose a change to approved info. It never changes what is live: it makes a draft version a person approves in the console, because approved info changes every future answer. based_on_version must be the latest approved version (from info_get), so you cannot overwrite newer work; if someone approves another version first, yours can no longer be approved.",
		schema: object(map[string]any{
			"based_on_version": map[string]any{"type": "integer", "minimum": 0, "description": "The version you read with info_get (0 if there is none yet)."},
			"set":              map[string]any{"type": "object", "description": "Top-level sections to replace or add, by key, with their whole new value; null removes one. E.g. {\"delivery_lead_time\": \"Next business day\"}."},
			"articles": map[string]any{"type": "array", "description": "Articles to add, change or remove, matched by title (case does not matter).", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":    str("The article's title."),
					"content":  str("Its full new text. Required for a new article."),
					"channels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Where it applies, e.g. [\"phone\"]; empty for everywhere."},
					"remove":   map[string]any{"type": "boolean", "description": "Remove this article."},
				},
				"required":             []string{"title"},
				"additionalProperties": false,
			}},
			"reason": str("For the person approving: what changed and why, and where it came from."),
		}, "based_on_version", "reason"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var p agent.InfoProposal
			if err := decodeArgs(raw, &p); err != nil {
				return nil, err
			}
			return agent.ProposeInfo(ctx, s.DB, a, p)
		},
	},
	"sms_status": {
		description: "Why texts are or are not going out: the workspace's texting numbers, and its 10DLC brand and campaigns as SignalWire's registry reports them right now, with the next step in plain words.",
		schema:      object(map[string]any{}),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, _ json.RawMessage) (any, error) {
			return sending.ReadSMSStatus(ctx, s.DB, s.Registry, a.WorkspaceID)
		},
	},
	"proposals_get": {
		description: "One proposal and what became of it: yours (a rejection's decision_note is the person's feedback), or anyone's in the workspace.",
		schema:      object(map[string]any{"proposal_id": str("The proposal_id from threads_draft or proposals_list.")}, "proposal_id"),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				ProposalID string `json:"proposal_id"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return agent.GetProposal(ctx, s.DB, a, args.ProposalID)
		},
	},
	"contacts_search": {
		description: "Find contacts by name, email address or phone number (at least 2 characters).",
		schema:      object(map[string]any{"query": str("Part of a name, email or phone number.")}, "query"),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				Query string `json:"query"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			list, err := agent.SearchContacts(ctx, s.DB, a.WorkspaceID, args.Query)
			return map[string]any{"contacts": nonNil(list)}, err
		},
	},
	"events_since": {
		description: "Everything that happened in the workspace after a cursor, oldest first. Start with after=0, then pass back next_after to see only what is new.",
		schema: object(map[string]any{
			"after": map[string]any{"type": "integer", "minimum": 0, "description": "The next_after from the previous call."},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		}),
		readOnly: true,
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				After int64 `json:"after"`
				Limit int   `json:"limit"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			list, next, err := agent.Events(ctx, s.DB, a.WorkspaceID, args.After, args.Limit)
			return map[string]any{"events": nonNil(list), "next_after": next}, err
		},
	},
	"call_flows_list": {
		description: "Every phone number and what it does with an incoming call: the active flow, any change waiting for a person, and which answer modes this server can run.",
		schema:      object(map[string]any{}),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, _ json.RawMessage) (any, error) {
			flows, err := voice.List(ctx, s.DB, a.WorkspaceID)
			for i := range flows {
				flows[i].Active.Summary = flows[i].Active.Flow.Describe()
			}
			return map[string]any{"numbers": flows, "available_modes": voice.Available}, err
		},
	},
	"call_flow_set": {
		description: "Change what a number does with incoming calls. Agree the change with the person first. In approve mode it waits for a person to activate it in the console; in autonomous mode it applies at once. Every unanswered call ends in voicemail.",
		schema: object(map[string]any{
			"number": str("The phone number, as call_flows_list shows it (E.164, e.g. +13105550100)."),
			"flow":   flowSchema,
		}, "number", "flow"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			var args struct {
				Number string     `json:"number"`
				Flow   voice.Flow `json:"flow"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			v, err := voice.SaveAs(ctx, s.DB, a.WorkspaceID, args.Number, a.ID, a.Autonomy, args.Flow)
			if err != nil {
				return nil, err
			}
			result := "active: calls use it now"
			if v.Status == "pending" {
				result = "waiting for a person to activate it in the console"
			}
			return map[string]any{"version": v.Version, "status": v.Status, "result": result, "summary": v.Flow.Describe()}, nil
		},
	},
	"notifications_get": {
		description: "The workspace's people and what each is told about, per category: instant (email and/or text), in a daily summary email, or off.",
		schema:      object(map[string]any{}),
		readOnly:    true,
		call: func(ctx context.Context, s *Server, a agent.Agent, _ json.RawMessage) (any, error) {
			people, err := notify.People(ctx, s.DB, a.WorkspaceID)
			return map[string]any{"people": people, "categories": notify.Categories}, err
		},
	},
	"notifications_set": {
		description: "Change what one person is told about, once they have asked for it. Only the categories given change. Texts need their mobile number in phone.",
		schema: object(map[string]any{
			"person_id": str("The person's id, from notifications_get."),
			"settings": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"category": str("A category key from notifications_get, e.g. voicemail, missed_call, sms."),
					"delivery": map[string]any{"type": "string", "enum": []string{notify.Instant, notify.Daily, notify.Off}},
					"channels": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"email", "sms"}}, "description": "For instant: email, sms or both. Default email."},
				},
				"required":             []string{"category", "delivery"},
				"additionalProperties": false,
			}},
			"phone": str("The person's mobile number for texts, E.164. Empty clears it."),
		}, "person_id"),
		call: func(ctx context.Context, s *Server, a agent.Agent, raw json.RawMessage) (any, error) {
			if a.Autonomy == "observe" {
				return nil, notify.ErrObserver
			}
			var args struct {
				PersonID string `json:"person_id"`
				notify.Change
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return notify.Update(ctx, s.DB, a.WorkspaceID, args.PersonID, a.ID, args.Change)
		},
	},
}

var action = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"mode":       map[string]any{"type": "string", "enum": []string{voice.Forward, voice.AI, voice.RingApp, voice.Voicemail}},
		"forward_to": str("For mode forward: the phone number to ring, E.164."),
		"then":       map[string]any{"type": "object", "description": "For mode forward or ring_app: the next step if nobody answers (or, for ring_app, nobody is online), with the same fields as this one, e.g. {\"mode\":\"forward\",\"forward_to\":\"+13105550100\",\"then\":{\"mode\":\"ai\"}}. At most 4 steps; without then, the caller leaves a message."},
	},
	"required":             []string{"mode"},
	"additionalProperties": false,
}

var flowSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"answer": map[string]any{
			"type":        "object",
			"description": "How the call is answered. mode is forward, ivr, ai, ring_app (rings everyone online in the console) or voicemail; only those in available_modes can be used.",
			"properties": map[string]any{
				"mode":       map[string]any{"type": "string", "enum": []string{voice.Forward, voice.IVR, voice.AI, voice.RingApp, voice.Voicemail}},
				"forward_to": str("For mode forward: the phone number to ring, E.164."),
				"then":       map[string]any{"type": "object", "description": "For mode forward or ring_app: the next step if nobody answers (or, for ring_app, nobody is online), with the same fields as this one, e.g. {\"mode\":\"forward\",\"forward_to\":\"+13105550100\",\"then\":{\"mode\":\"ai\"}}. At most 4 steps; without then, the caller leaves a message."},
			},
			"required":             []string{"mode"},
			"additionalProperties": false,
		},
		"menu": map[string]any{
			"type":        "object",
			"description": "For answer mode ivr: what the caller hears, the keys, and what happens if they press nothing.",
			"properties": map[string]any{
				"prompt": str("Read to the caller, e.g. \"For leasing press 1. For current residents press 2.\""),
				"options": map[string]any{"type": "array", "maxItems": 9, "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"digit":  str("One key, 0 to 9."),
						"label":  str("A short name, e.g. Leasing."),
						"action": action,
					},
					"required":             []string{"digit", "action"},
					"additionalProperties": false,
				}},
				"no_choice": action,
			},
			"required":             []string{"prompt", "options", "no_choice"},
			"additionalProperties": false,
		},
		"ring_seconds": map[string]any{"type": "integer", "minimum": 5, "maximum": 60, "description": "How long a forward rings before voicemail. Default 20."},
		"greeting":     str("Read before the voicemail beep."),
		"agent": map[string]any{
			"type":        "object",
			"description": "Required when any action is mode ai: how the AI answers. What it knows comes from the workspace's approved info, never from here.",
			"properties": map[string]any{
				"opening":      str("The AI's first line, spoken as it picks up, e.g. \"Thanks for calling Acme Plumbing, how can I help?\" At most 300 characters."),
				"instructions": str("This business's rules for calls, on top of the built-in phone manner: what to never promise, what to ask, when to take a message. At most 20000 characters."),
				"voice":        map[string]any{"type": "string", "enum": voice.Voices, "description": "Default marin."},
				"transfer_to":  str("A person's phone number (E.164) the AI may transfer urgent callers to. Empty: it takes a message instead."),
			},
			"required":             []string{"opening"},
			"additionalProperties": false,
		},
	},
	"required":             []string{"answer"},
	"additionalProperties": false,
}

var workspacesList = tool{
	description: "The businesses (workspaces) you act for, with your mode in each and what it means.",
	schema:      object(map[string]any{}),
	readOnly:    true,
}

// toolList is every tool. When the caller acts for several workspaces,
// each tool but workspaces_list requires a workspace argument.
func toolList(multi bool, workspaces string) []map[string]any {
	names := make([]string, 0, len(tools)+1)
	for name := range tools {
		names = append(names, name)
	}
	names = append(names, "workspaces_list")
	sort.Strings(names)
	list := make([]map[string]any, 0, len(names))
	for _, name := range names {
		t, ok := tools[name]
		if !ok {
			t = workspacesList
		}
		schema := t.schema
		if multi && name != "workspaces_list" {
			schema = withWorkspace(schema, workspaces)
		}
		list = append(list, map[string]any{
			"name":        name,
			"description": t.description,
			"inputSchema": schema,
			"annotations": map[string]any{"readOnlyHint": t.readOnly, "openWorldHint": false},
		})
	}
	return list
}

// withWorkspace copies a schema with a required workspace property.
func withWorkspace(schema map[string]any, workspaces string) map[string]any {
	out := map[string]any{}
	for k, v := range schema {
		out[k] = v
	}
	props := map[string]any{"workspace": str("Which business this is for: its id or exact name, from workspaces_list. One of: " + workspaces + ".")}
	if p, ok := schema["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	out["properties"] = props
	required := []string{"workspace"}
	if r, ok := schema["required"].([]string); ok {
		required = append(required, r...)
	}
	out["required"] = required
	return out
}

func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
