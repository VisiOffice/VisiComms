import {useCallback, useEffect, useRef, useState} from 'react';
import {useNavigate} from 'react-router-dom';
import {Relay} from '@signalwire/js';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {HStack, VStack} from '@astryxdesign/core/Layout';
import {StatusDot} from '@astryxdesign/core/StatusDot';
import {Switch} from '@astryxdesign/core/Switch';
import {Text} from '@astryxdesign/core/Text';

import {api, type LiveCall, type PhoneToken} from '../api';

// The browser phone. While it is online, calls to a number whose flow rings
// the app ring here (and in every other online console); whoever answers
// first takes the call. Being online is remembered per browser.

type SWCall = {
  id: string;
  state: string;
  direction?: string;
  options: {remoteCallerNumber?: string; remoteCallerName?: string; destinationNumber?: string};
  answer: () => Promise<void> | void;
  hangup: () => void;
  muteAudio: () => void;
  unmuteAudio: () => void;
};
type RelayNotice = {type: string; call?: SWCall};
type Client = {
  connect: () => Promise<void>;
  disconnect: () => Promise<void>;
  on: (event: string, fn: (arg: never) => void) => void;
  refreshToken: (token: string) => Promise<void>;
  remoteElement?: HTMLMediaElement;
  newCall: (options: {destinationNumber: string; callerNumber: string; audio: boolean; video: boolean}) => Promise<unknown>;
};

// The connected phone, for placing calls from elsewhere in the console.
let connected: Client | null = null;

// dialOut calls a number from the browser phone, showing one of the
// workspace's numbers. The phone must be online.
export async function dialOut(to: string, from: string): Promise<void> {
  if (!connected) throw new Error('Turn on “Take calls here” first.');
  await connected.newCall({destinationNumber: to, callerNumber: from, audio: true, video: false});
}

const ONLINE_KEY = 'visicomms.phone.online';
const HEARTBEAT_MS = 30_000;
const REFRESH_MS = 50 * 60_000;
const ENDED = new Set(['hangup', 'destroy', 'purge']);

function remembered(): boolean {
  try {
    return localStorage.getItem(ONLINE_KEY) === '1';
  } catch {
    return false;
  }
}

function remember(online: boolean) {
  try {
    localStorage.setItem(ONLINE_KEY, online ? '1' : '0');
  } catch {
    // Private windows may refuse storage; the switch still works this session.
  }
}

// The caller's audio plays through a hidden element the SDK writes to.
function remoteAudio(): HTMLAudioElement {
  const id = 'visicomms-phone-audio';
  let el = document.getElementById(id) as HTMLAudioElement | null;
  if (!el) {
    el = document.createElement('audio');
    el.id = id;
    el.autoplay = true;
    document.body.appendChild(el);
  }
  return el;
}

// A ringtone made in the browser, so there is no sound file to ship.
function startRinging(ctx: AudioContext): () => void {
  let stopped = false;
  const ring = () => {
    if (stopped) return;
    for (const [freq, at] of [
      [440, 0],
      [480, 0],
    ]) {
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();
      osc.frequency.value = freq;
      gain.gain.value = 0.05;
      osc.connect(gain).connect(ctx.destination);
      osc.start(ctx.currentTime + at);
      osc.stop(ctx.currentTime + at + 1.5);
    }
  };
  ring();
  const timer = setInterval(ring, 4000);
  return () => {
    stopped = true;
    clearInterval(timer);
  };
}

function formatNumber(n: string) {
  const m = /^\+1(\d{3})(\d{3})(\d{4})$/.exec(n);
  return m ? `(${m[1]}) ${m[2]}-${m[3]}` : n;
}

export function Softphone() {
  const navigate = useNavigate();
  const [online, setOnline] = useState(remembered);
  const [status, setStatus] = useState<'off' | 'connecting' | 'ready' | 'error'>('off');
  const [error, setError] = useState('');
  const [call, setCall] = useState<SWCall | null>(null);
  const [caller, setCaller] = useState<LiveCall | null>(null);
  const [muted, setMuted] = useState(false);
  const client = useRef<Client | null>(null);
  const audio = useRef<AudioContext | null>(null);
  const stopRing = useRef<(() => void) | null>(null);

  const silence = () => {
    stopRing.current?.();
    stopRing.current = null;
  };

  const onCall = useCallback((c: SWCall) => {
    if (ENDED.has(c.state)) {
      silence();
      setCall(cur => (cur?.id === c.id ? null : cur));
      setMuted(false);
      return;
    }
    if (c.direction === 'outbound') setCaller(null);
    setCall({...c, answer: () => c.answer(), hangup: () => c.hangup(), muteAudio: () => c.muteAudio(), unmuteAudio: () => c.unmuteAudio()});
    if (c.state === 'ringing') {
      if (!stopRing.current && audio.current) stopRing.current = startRinging(audio.current);
      api
        .get<{calls: LiveCall[]}>('/phone/calls')
        .then(r => {
          const live = r.calls[0] ?? null;
          setCaller(live);
          if ('Notification' in window && Notification.permission === 'granted' && document.hidden) {
            new Notification('Incoming call', {body: live?.contact || formatNumber(live?.from ?? c.options.remoteCallerNumber ?? '')});
          }
        })
        .catch(() => setCaller(null));
    } else {
      silence();
    }
  }, []);

  useEffect(() => {
    if (!online) return;
    let cancelled = false;
    let heartbeat: ReturnType<typeof setInterval> | undefined;
    let refresh: ReturnType<typeof setInterval> | undefined;
    setStatus('connecting');
    setError('');
    (async () => {
      try {
        const creds = await api.post<PhoneToken>('/phone/token');
        if (!creds.available) throw new Error(creds.reason ?? 'The phone is not available.');
        await navigator.mediaDevices.getUserMedia({audio: true}).then(s => s.getTracks().forEach(t => t.stop()));
        if (cancelled) return;
        const c = new Relay({project: creds.project!, token: creds.token!}) as unknown as Client;
        c.remoteElement = remoteAudio();
        c.on('signalwire.ready', () => {
          if (cancelled) return;
          setStatus('ready');
          connected = c;
          const beat = () => api.put('/phone/presence', {online: true}).catch(() => undefined);
          beat();
          heartbeat = setInterval(beat, HEARTBEAT_MS);
        });
        c.on('signalwire.error', (e: never) => {
          setStatus('error');
          setError(String((e as {message?: string})?.message ?? 'The phone lost its connection.'));
        });
        c.on('signalwire.notification', (n: never) => {
          const note = n as RelayNotice;
          if (note.type === 'callUpdate' && note.call) onCall(note.call);
        });
        client.current = c;
        await c.connect();
        refresh = setInterval(async () => {
          const next = await api.post<PhoneToken>('/phone/token').catch(() => null);
          if (next?.token) await c.refreshToken(next.token).catch(() => undefined);
        }, REFRESH_MS);
      } catch (e) {
        if (cancelled) return;
        setStatus('error');
        setError(e instanceof Error ? e.message : String(e));
      }
    })();
    // Closing the tab says so at once; otherwise presence lapses in 75 seconds.
    const away = () =>
      void fetch('/api/phone/presence', {method: 'PUT', keepalive: true, headers: {'Content-Type': 'application/json'}, body: JSON.stringify({online: false})});
    window.addEventListener('pagehide', away);
    return () => {
      cancelled = true;
      clearInterval(heartbeat);
      clearInterval(refresh);
      window.removeEventListener('pagehide', away);
      silence();
      api.put('/phone/presence', {online: false}).catch(() => undefined);
      client.current?.disconnect().catch(() => undefined);
      client.current = null;
      connected = null;
      setStatus('off');
      setCall(null);
    };
  }, [online, onCall]);

  const toggle = (on: boolean) => {
    if (on) {
      // Sound and notifications need a click to be allowed.
      audio.current ??= new AudioContext();
      void audio.current.resume();
      if ('Notification' in window && Notification.permission === 'default') void Notification.requestPermission();
    }
    remember(on);
    setOnline(on);
  };

  const answer = async () => {
    if (!call) return;
    silence();
    await call.answer();
    if (caller) navigate(`/threads/${caller.thread_id}`);
  };

  const outbound = call?.direction === 'outbound';
  const who = outbound
    ? formatNumber(call?.options.destinationNumber ?? '')
    : caller?.contact && caller.contact !== caller.from
      ? caller.contact
      : formatNumber(caller?.from ?? call?.options.remoteCallerNumber ?? '');

  return (
    <VStack gap={2}>
      <HStack gap={2} vAlign="center">
        <StatusDot variant={status === 'ready' ? 'success' : status === 'error' ? 'error' : 'neutral'} label={status === 'ready' ? 'Online' : status === 'connecting' ? 'Connecting' : status === 'error' ? 'Not connected' : 'Away'} />
        <Switch label="Take calls here" value={online} onChange={toggle} size="sm" />
      </HStack>
      {status === 'error' && error && <Banner status="error" title={error} />}
      {call && (
        <Card padding={3}>
          <VStack gap={2}>
            <Text weight="semibold">{outbound ? (call.state === 'active' ? 'On a call' : 'Calling') : call.state === 'ringing' ? 'Incoming call' : 'On a call'}</Text>
            <Text>{who || 'Unknown caller'}</Text>
            {caller && <Text type="supporting">To {formatNumber(caller.to)}</Text>}
            <HStack gap={2} wrap="wrap">
              {call.state === 'ringing' && !outbound ? (
                <>
                  <Button label="Answer" variant="primary" size="sm" onClick={() => void answer()} />
                  <Button label="Decline" size="sm" onClick={() => call.hangup()} />
                </>
              ) : (
                <>
                  <Button
                    label={muted ? 'Unmute' : 'Mute'}
                    size="sm"
                    onClick={() => {
                      if (muted) call.unmuteAudio();
                      else call.muteAudio();
                      setMuted(!muted);
                    }}
                  />
                  <Button label="Hang up" variant="destructive" size="sm" onClick={() => call.hangup()} />
                </>
              )}
            </HStack>
          </VStack>
        </Card>
      )}
    </VStack>
  );
}
