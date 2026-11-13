// Turns a parsed message into the JSON POST /webhooks/email accepts, and signs
// it the way that endpoint checks: hex HMAC-SHA256 of "<timestamp>.<body>".
// Kept free of Worker bindings so it can be unit-tested.

import type {Address, Email} from 'postal-mime';

export type AttachmentRef = {
  key: string;
  filename: string;
  content_type: string;
  byte_size: number;
  checksum: string;
};

export type Envelope = {
  // The address the message was delivered to, e.g. in+acme-plumbing-6d97…@in.visicomms.com.
  // It selects the workspace, so it is the envelope recipient, not the To header
  // (a forwarded message's To header is the business's own address).
  to: string;
  from: string;
  rawKey: string;
};

function formatAddress(a: Address | undefined): string {
  if (!a || !('address' in a) || !a.address) {
    return '';
  }
  return a.name ? `${a.name.replace(/"/g, '')} <${a.address}>` : a.address;
}

export function buildPayload(email: Email, env: Envelope, attachments: AttachmentRef[]): Record<string, unknown> {
  // First occurrence of each header, lower-cased: the API reads auto-submitted,
  // precedence, x-failed-recipients, content-type and the like from here.
  const headers: Record<string, string> = {};
  for (const h of email.headers) {
    if (!(h.key in headers)) {
      headers[h.key] = h.value;
    }
  }
  return {
    messageId: email.messageId ?? '',
    to: env.to,
    from: formatAddress(email.from) || env.from,
    replyTo: formatAddress(email.replyTo?.[0]),
    subject: email.subject ?? '',
    text: email.text ?? '',
    html: email.html ?? '',
    inReplyTo: email.inReplyTo ?? '',
    references: email.references ?? '',
    // The original Date header (RFC 5322), which the API parses; postal-mime's
    // own date field is reformatted.
    date: headers['date'] ?? '',
    headers,
    attachments,
    raw_key: env.rawKey,
    envelope_from: env.from,
  };
}

export function envelope(payload: Record<string, unknown>): string {
  return JSON.stringify({event_type: 'email.received', payload});
}

const encoder = new TextEncoder();

function hex(buf: ArrayBuffer): string {
  return [...new Uint8Array(buf)].map(b => b.toString(16).padStart(2, '0')).join('');
}

export async function sign(secret: string, timestamp: string, body: string): Promise<string> {
  const key = await crypto.subtle.importKey('raw', encoder.encode(secret), {name: 'HMAC', hash: 'SHA-256'}, false, ['sign']);
  return hex(await crypto.subtle.sign('HMAC', key, encoder.encode(`${timestamp}.${body}`)));
}

export async function sha256(data: ArrayBuffer | Uint8Array): Promise<string> {
  return hex(await crypto.subtle.digest('SHA-256', data));
}

// What to do with the API's answer. A 404 means no workspace owns the
// address, so the sender gets a bounce instead of silence; 400 and 422 will
// not get better by retrying; anything else (5xx, 401 from a secret being
// rotated, a timeout) is retried from the queue.
export function outcome(status: number): 'delivered' | 'reject' | 'drop' | 'retry' {
  if (status >= 200 && status < 300) {
    return 'delivered';
  }
  if (status === 404) {
    return 'reject';
  }
  if (status === 400 || status === 413 || status === 422) {
    return 'drop';
  }
  return 'retry';
}
