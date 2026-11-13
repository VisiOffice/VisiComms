// The Email Worker behind in.visicomms.com. Email Routing hands it every
// message for in+…@in.visicomms.com. It stores the raw message first, so a
// message is never lost, then posts the parsed message to the API, signed.
// When the API can't take it, the message waits in a queue and is retried.

import PostalMime from 'postal-mime';
import {buildPayload, envelope, outcome, sha256, sign, type AttachmentRef} from './payload';

interface Env {
  MAIL: R2Bucket;
  RETRY: Queue<{bodyKey: string}>;
  // e.g. https://api.visicomms.com
  API_ORIGIN: string;
  WEBHOOK_SECRET: string;
}

async function deliver(env: Env, body: string): Promise<number> {
  const timestamp = String(Math.floor(Date.now() / 1000));
  try {
    const res = await fetch(new URL('/webhooks/email', env.API_ORIGIN), {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-VisiComms-Timestamp': timestamp,
        'X-VisiComms-Signature': await sign(env.WEBHOOK_SECRET, timestamp, body),
      },
      body,
    });
    return res.status;
  } catch (err) {
    console.error('delivery failed', err);
    return 0;
  }
}

function today(): string {
  return new Date().toISOString().slice(0, 10).replace(/-/g, '/');
}

export default {
  async email(message: ForwardableEmailMessage, env: Env): Promise<void> {
    const id = crypto.randomUUID();
    const day = today();
    const raw = await new Response(message.raw).arrayBuffer();
    const rawKey = `raw/${day}/${id}.eml`;
    await env.MAIL.put(rawKey, raw, {httpMetadata: {contentType: 'message/rfc822'}});

    const email = await PostalMime.parse(raw);

    // Attachments are stored once, by content hash, and the API gets
    // pointers; the bytes never travel in the webhook.
    const attachments: AttachmentRef[] = [];
    for (const a of email.attachments) {
      const content = typeof a.content === 'string' ? new TextEncoder().encode(a.content) : a.content;
      const checksum = await sha256(content);
      const key = `attachments/${checksum}`;
      if (!(await env.MAIL.head(key))) {
        await env.MAIL.put(key, content, {httpMetadata: {contentType: a.mimeType}});
      }
      attachments.push({key, filename: a.filename ?? 'attachment', content_type: a.mimeType, byte_size: content.byteLength, checksum});
    }

    const body = envelope(buildPayload(email, {to: message.to, from: message.from, rawKey}, attachments));
    // Kept so a retry sends exactly what was first built.
    const bodyKey = `payloads/${day}/${id}.json`;
    await env.MAIL.put(bodyKey, body, {httpMetadata: {contentType: 'application/json'}});

    const status = await deliver(env, body);
    switch (outcome(status)) {
      case 'delivered':
        return;
      case 'reject':
        message.setReject('No VisiComms workspace receives mail at this address.');
        return;
      case 'drop':
        console.error('API refused message', {status, rawKey});
        return;
      case 'retry':
        console.warn('API unavailable, queued for retry', {status, rawKey});
        await env.RETRY.send({bodyKey});
    }
  },

  async queue(batch: MessageBatch<{bodyKey: string}>, env: Env): Promise<void> {
    for (const msg of batch.messages) {
      const stored = await env.MAIL.get(msg.body.bodyKey);
      if (!stored) {
        console.error('retry payload missing', msg.body.bodyKey);
        msg.ack();
        continue;
      }
      const status = await deliver(env, await stored.text());
      if (outcome(status) === 'retry') {
        // 30 s, 1 min, 2 min … capped at an hour. The API ignores a second
        // delivery of the same Message-ID, so a retry can't double a thread.
        msg.retry({delaySeconds: Math.min(3600, 30 * 2 ** Math.min(msg.attempts, 7))});
      } else {
        msg.ack();
      }
    }
  },
} satisfies ExportedHandler<Env, {bodyKey: string}>;
