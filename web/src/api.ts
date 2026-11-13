// The console's API client and the shapes it returns. Every call goes to
// /api on the page's own origin, so the session cookie rides along.

export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method,
    credentials: 'same-origin',
    headers: body === undefined && method === 'GET' ? {} : {'Content-Type': 'application/json'},
    body: method === 'GET' ? undefined : JSON.stringify(body ?? {}),
  });
  const data = await res.json().catch(() => undefined);
  if (!res.ok) {
    throw new ApiError(res.status, data?.error ?? `Request failed (${res.status})`);
  }
  if (data === undefined) {
    // A page instead of JSON means the request never reached the API (a
    // proxy or the static site answered), so nothing it says can be trusted.
    throw new ApiError(502, 'The server could not be reached. Try again in a moment.');
  }
  return data as T;
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
  delete: <T>(path: string) => request<T>('DELETE', path),
};

export type Person = {id: string; workspace_id: string; name: string; email: string};
export type Me = {person: Person; workspace: {id: string; name: string}};
export type Membership = {workspace_id: string; name: string; current: boolean};

export type AttentionKind = 'held_draft' | 'unanswered' | 'failed_run' | 'unknown_send' | 'send_failed' | 'complaint' | 'connector_error';

export type AttentionItem = {
  kind: AttentionKind;
  id: string;
  thread_id?: string;
  contact?: string;
  subject?: string;
  reason: string;
  waiting_since: string;
  due_at?: string;
};

export type Identity = {channel: string; address: string; verified: boolean};

export type TimelineEntry = {
  id: number;
  type: string;
  at: string;
  actor?: string;
  actor_kind?: 'human' | 'agent' | '';
  untrusted: boolean;
  suspicious: boolean;
  payload: Record<string, unknown>;
};

export type Check = {label: string; ok: boolean};

export type Draft = {
  id: string;
  status: string;
  action: string;
  agent: string;
  payload: {subject: string; body: string; quick_replies?: string[]};
  recipients: {email?: string; phone?: string; name?: string; chat_visitor?: string}[];
  rule: string;
  reason: string;
  checks: Check[];
  approved_info_version?: number;
  created_at: string;
};

export type Link = {system: string; record_type: string; external_id: string; state: string; pushed_at?: string};

export type Thread = {
  id: string;
  visitor_typing?: boolean;
  subject: string;
  channel: string;
  phone_number?: string;
  remote_address?: string;
  state: 'open' | 'waiting' | 'resolved';
  taken_over_by?: string;
  taken_over_by_id?: string;
  version: number;
  contact: {
    id: string;
    name: string;
    tags: string[];
    trusted: boolean;
    memory: {text: string; source: string}[] | null;
    identities: Identity[] | null;
    opted_out: string[] | null;
  };
  timeline: TimelineEntry[] | null;
  proposals: Draft[] | null;
  links: Link[] | null;
};

export type ApprovedInfoVersion = {
  id: string;
  version: number;
  content: ApprovedInfoContent;
  approved_by?: string;
  approved_at?: string;
  created_at: string;
  pending_quoting: number;
  proposed_by?: string;
  reason?: string;
  based_on?: number;
};

// Approved info is the business's own: titled articles, plus any other
// top-level sections it keeps (hours, service area, policies).
export type ApprovedInfoArticle = {title: string; content: string; channels?: string[]};
export type ApprovedInfoContent = {articles?: ApprovedInfoArticle[]} & Record<string, unknown>;

export type Connections = {
  connectors: {id: string; kind: string; name: string; health: 'ok' | 'failing' | 'unknown'; last_error?: string; fix_owner?: string; checked_at?: string}[] | null;
  inboxes: string[] | null;
  systems: {system: string; records: number; last_pushed?: string}[] | null;
  agents: {id: string; name: string; autonomy: 'observe' | 'approve' | 'autonomous'; config_version?: string; revoked_at?: string; may_send: boolean; can_send: boolean}[] | null;
};

export type SetupStep = {
  key: 'inbox' | 'sending' | 'approved_info' | 'agent' | 'team';
  title: string;
  state: 'todo' | 'partial' | 'done';
  detail: string;
  optional: boolean;
  value?: string;
};
export type SetupStatus = {steps: SetupStep[]; complete: boolean};

export type SMSSettings = {
 available: boolean;
 numbers: {number: string; capabilities: Record<string, boolean>}[];
 inbound_url: string;
 status_url: string;
};

export type FaxSettings = {available: boolean; numbers: {number: string; capabilities: Record<string, boolean>}[]; inbound_url: string};
export type FaxDocument = {id: string; filename: string; sha256: string; size: number};
export type FaxResult = {action_id: string; thread_id: string; state: string};

// Call flows: what each number does with an incoming call.
export type CallMode = 'forward' | 'ivr' | 'ai' | 'ring_app' | 'voicemail';
export type CallAction = {mode: CallMode; forward_to?: string; then?: CallAction};
export type MenuOption = {digit: string; label: string; action: CallAction};
export type CallMenu = {prompt: string; options: MenuOption[]; no_choice: CallAction};
// The console's browser phone.
export type PhoneToken = {available: boolean; reason?: string; project?: string; token?: string; resource?: string};
export type LiveCall = {call_id: string; thread_id: string; from: string; to: string; contact: string; started_at: string};
export type CallAgent = {opening: string; instructions: string; voice: string; transfer_to?: string};
export type CallFlow = {answer: CallAction; menu?: CallMenu; ring_seconds: number; greeting: string; agent?: CallAgent};
export type CallFlowVersion = {id?: string; number: string; version: number; status: 'active' | 'pending'; flow: CallFlow; created_by?: string};
export type NumberFlows = {number: string; active: CallFlowVersion; pending?: CallFlowVersion};
export type NumbersResponse = {numbers: NumberFlows[]; available_modes: Partial<Record<CallMode, boolean>>; voices: string[]};

// Entrances: door intercoms whose calls the AI answers as a gatekeeper.
// password is only present right after the SIP account is made.
export type SIPAccount = {server: string; port: number; transport: string; username: string; password?: string; recipient: string};
export type Entrance = {id: string; name: string; caller_id?: string; flow: CallFlow; contact_id?: string; sip?: SIPAccount};
export type EntrancesResponse = {
  entrances: Entrance[];
  sip_available: boolean;
  available_modes: Partial<Record<CallMode, boolean>>;
  voices: string[];
  default_flow: CallFlow;
};

// Postal mail through the workspace's own Lob account.
export type PostalAddress = {name: string; company?: string; line1: string; line2?: string; city: string; state: string; zip: string};
export type SavedAddress = PostalAddress & {id: string; deliverability: string; returned: boolean; verified: boolean};
export type MailSettings = {
  connector_id: string;
  mode: 'test' | 'live';
  return_address: PostalAddress;
  from: string;
  monthly_cap_cents: number;
  health: 'ok' | 'failing' | 'unknown';
  last_error?: string;
  has_webhook_secret: boolean;
  webhook_url: string;
};
export type MailStatus = {available: boolean; connected: boolean; settings?: MailSettings; month_spent_cents?: number; default_cap_cents?: number};
export type ExtraService = '' | 'certified' | 'certified_return_receipt';
export type LetterOptions = {extra_service: ExtraService; color: boolean; double_sided: boolean};
export type LetterEstimate = {pages: number; sheets: number; cost_cents: number};
export type LetterResult = LetterEstimate & {action_id: string; thread_id: string; state: string};

// Notifications: what each person is told about, and how.
export type Delivery = 'instant' | 'daily' | 'off';
export type NotifyChannel = 'email' | 'sms';
export type NotificationSetting = {category: string; delivery: Delivery; channels: NotifyChannel[]};
export type NotificationCategory = {key: string; label: string; description: string; default: Delivery};
export type NotifiedPerson = {id: string; name: string; email: string; phone?: string; settings: NotificationSetting[]};
export type NotificationsResponse = {people: NotifiedPerson[]; categories: NotificationCategory[]; channels: Record<NotifyChannel, boolean>};

export type ChatAsk = 'off' | 'optional' | 'required';

export type ChatWidget = {
  id: string;
  public_id: string;
  name: string;
  color: string;
  position: 'right' | 'left';
  greeting: string;
  returning_greeting: string;
  reply_time: string;
  offline_message: string;
  ask_name: ChatAsk;
  ask_email: ChatAsk;
  ask_phone: ChatAsk;
  allowed_origins: string[];
  quick_replies: string[];
  disabled: boolean;
  created_at: string;
};

export type ChatWidgets = {widgets: ChatWidget[]; script_url: string};

export type ChatPopupTrigger = 'delay' | 'page' | 'exit' | 'scroll' | 'idle';

export type ChatPopupSettings = {
  position: number;
  name: string;
  text: string;
  sender_name: string;
  trigger: ChatPopupTrigger;
  seconds: number;
  scroll_percent: number;
  page_pattern: string;
  opens_window: boolean;
  sound: boolean;
  devices: 'all' | 'desktop' | 'mobile';
  frequency: 'session' | 'first_visit';
  only_if_none: boolean;
  quick_replies: string[];
  active: boolean;
};

export type ChatPopup = ChatPopupSettings & {id: string; widget_id: string; shown: number; chats: number};

export type SlackMember = {id: string; name: string; email: string; slack_user_id: string};
export type SlackSettings = {
  connected: boolean;
  available: boolean;
  settings: {channel_id: string; mention: string; mention_on: 'new' | 'every' | 'never'; team_name?: string};
  health: 'ok' | 'failing' | 'unknown' | '';
  last_error: string;
  events_url: string;
  members: SlackMember[];
};

export type OAuthRequest = {client_name: string; redirect_host: string; workspaces: {id: string; name: string}[]};
