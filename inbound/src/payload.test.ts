import {describe, expect, it} from 'vitest';
import PostalMime from 'postal-mime';
import {buildPayload, envelope, outcome, sign} from './payload';

const raw = [
  'From: "Marcus Lee" <marcus@roofco.example>',
  'To: quotes@acmeplumbing.example',
  'Subject: water heater for a remodel?',
  'Message-ID: <m2@roofco.example>',
  'In-Reply-To: <m1@roofco.example>',
  'References: <m0@roofco.example> <m1@roofco.example>',
  'Date: Thu, 08 Oct 2026 09:14:00 -0700',
  'Auto-Submitted: no',
  'MIME-Version: 1.0',
  'Content-Type: multipart/mixed; boundary="b"',
  '',
  '--b',
  'Content-Type: text/plain; charset=utf-8',
  'Content-Transfer-Encoding: quoted-printable',
  '',
  'Do you have a water heater? Price =3D ?',
  '--b',
  'Content-Type: application/pdf; name="site.pdf"',
  'Content-Disposition: attachment; filename="site.pdf"',
  'Content-Transfer-Encoding: base64',
  '',
  'JVBERi0xLjQK',
  '--b--',
  '',
].join('\r\n');

describe('buildPayload', () => {
  it('maps a forwarded message onto the API payload', async () => {
    const email = await PostalMime.parse(raw);
    const p = buildPayload(email, {to: 'in+acmeplumbing-6d97e29a@in.visicomms.com', from: 'bounce@gmail.example', rawKey: 'raw/x.eml'}, []);
    expect(p.to).toBe('in+acmeplumbing-6d97e29a@in.visicomms.com');
    expect(p.from).toBe('Marcus Lee <marcus@roofco.example>');
    expect(p.subject).toBe('water heater for a remodel?');
    expect(p.messageId).toBe('<m2@roofco.example>');
    expect(p.inReplyTo).toBe('<m1@roofco.example>');
    expect(p.references).toBe('<m0@roofco.example> <m1@roofco.example>');
    expect(p.date).toBe('Thu, 08 Oct 2026 09:14:00 -0700');
    expect((p.text as string).trim()).toBe('Do you have a water heater? Price = ?');
    expect((p.headers as Record<string, string>)['auto-submitted']).toBe('no');
    expect(email.attachments).toHaveLength(1);
    expect(email.attachments[0].filename).toBe('site.pdf');
  });

  it('falls back to the envelope sender when there is no From header', async () => {
    const email = await PostalMime.parse('Subject: hi\r\n\r\nbody');
    const p = buildPayload(email, {to: 'in+a@in.visicomms.com', from: 'someone@example.com', rawKey: 'k'}, []);
    expect(p.from).toBe('someone@example.com');
  });
});

describe('sign', () => {
  it('matches the HMAC the API checks', async () => {
    const body = envelope({subject: 'x'});
    // Same inputs as in Go: hex(HMAC-SHA256("secret", "1700000000." + body)).
    const sig = await sign('secret', '1700000000', body);
    const {createHmac} = await import('node:crypto');
    expect(sig).toBe(createHmac('sha256', 'secret').update(`1700000000.${body}`).digest('hex'));
  });
});

describe('outcome', () => {
  it('bounces unknown addresses, retries outages, drops bad requests', () => {
    expect(outcome(200)).toBe('delivered');
    expect(outcome(404)).toBe('reject');
    expect(outcome(422)).toBe('drop');
    expect(outcome(500)).toBe('retry');
    expect(outcome(401)).toBe('retry');
    expect(outcome(0)).toBe('retry');
  });
});
