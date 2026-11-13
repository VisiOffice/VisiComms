import {startAuthentication, startRegistration} from '@simplewebauthn/browser';
import {api, type Membership, type Person} from './api';

type Ceremony = {ceremony_id: string; options: {publicKey: never}};

// Makes a passkey from an invite and signs in with it.
export async function registerPasskey(invite: string): Promise<Person> {
  const begin = await api.post<Ceremony>('/auth/register/begin', {invite});
  const credential = await startRegistration({optionsJSON: begin.options.publicKey});
  return api.post<Person>('/auth/register/finish', {ceremony_id: begin.ceremony_id, credential});
}

// Signs in with any passkey saved for this site; the browser asks which.
export async function signInWithPasskey(): Promise<Person> {
  const begin = await api.post<Ceremony>('/auth/login/begin');
  const credential = await startAuthentication({optionsJSON: begin.options.publicKey});
  return api.post<Person>('/auth/login/finish', {ceremony_id: begin.ceremony_id, credential});
}

// Signs in with another workspace's passkey to add that workspace to the
// one you are signed in to; the session stays where it is.
export async function linkPasskey(): Promise<Membership[]> {
  const begin = await api.post<Ceremony>('/workspaces/link/begin');
  const credential = await startAuthentication({optionsJSON: begin.options.publicKey});
  const res = await api.post<{workspaces: Membership[]}>('/workspaces/link/finish', {ceremony_id: begin.ceremony_id, credential});
  return res.workspaces;
}

// The browser's own wording when someone closes the passkey prompt is not
// helpful; say what happened instead.
export function passkeyErrorMessage(err: unknown): string {
  if (err instanceof Error && (err.name === 'NotAllowedError' || err.name === 'AbortError')) {
    return 'The passkey prompt was closed. Try again when you are ready.';
  }
  return err instanceof Error ? err.message : 'Something went wrong. Try again.';
}
