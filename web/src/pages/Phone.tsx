import {useEffect, useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {Layout, LayoutContent, LayoutHeader, HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {RadioList, RadioListItem} from '@astryxdesign/core/RadioList';
import {Selector} from '@astryxdesign/core/Selector';
import {Spinner} from '@astryxdesign/core/Spinner';
import {TextArea} from '@astryxdesign/core/TextArea';
import {TextInput} from '@astryxdesign/core/TextInput';

import {Entrances} from '../components/Entrances';
import {api, type CallAction, type CallAgent, type CallFlow, type CallMode, type NumberFlows, type NumbersResponse} from '../api';

const MODES: {mode: CallMode; label: string; description: string}[] = [
  {mode: 'ai', label: 'AI answers', description: 'The voice agent answers, helps the caller, and takes a message or transfers.'},
  {mode: 'ring_app', label: 'Ring in the app', description: 'Rings everyone who has “Take calls here” on in the console; the first to answer takes it.'},
  {mode: 'forward', label: 'Forward to a number', description: 'Rings a phone number, such as your mobile.'},
  {mode: 'ivr', label: 'Menu (IVR)', description: '“Press 1 for…”: each key forwards, goes to the AI, or takes a message.'},
  {mode: 'voicemail', label: 'Voicemail only', description: 'Plays your greeting and records a message.'},
];

const OPTION_MODES: CallMode[] = ['ai', 'ring_app', 'forward', 'voicemail'];

function label(mode: CallMode) {
  return MODES.find(m => m.mode === mode)?.label ?? mode;
}

function formatNumber(n: string) {
  const m = /^\+1(\d{3})(\d{3})(\d{4})$/.exec(n);
  return m ? `(${m[1]}) ${m[2]}-${m[3]}` : n;
}

// A ring or forward can go unanswered, so it can have a next step.
const RINGS: CallMode[] = ['ring_app', 'forward'];
const MAX_STEPS = 4;

function steps(a: CallAction): CallAction[] {
  return a.then ? [a, ...steps(a.then)] : [a];
}

export function usesAI(f: CallFlow) {
  const starts = [f.answer, ...(f.menu ? [f.menu.no_choice, ...f.menu.options.map(o => o.action)] : [])];
  return starts.some(a => steps(a).some(s => s.mode === 'ai'));
}

export function describe(f: CallFlow): string {
  const one = (a: CallAction) => (a.mode === 'forward' ? `Forward to ${formatNumber(a.forward_to ?? '')}` : label(a.mode));
  const chain = (a: CallAction) => steps(a).map(one).join(', then ');
  if (f.answer.mode !== 'ivr' || !f.menu) return chain(f.answer);
  return `Menu: ${f.menu.options.map(o => `${o.digit} ${o.label || chain(o.action)}`).join(', ')}`;
}

export function PhonePage() {
  const q = useQuery({queryKey: ['numbers'], queryFn: () => api.get<NumbersResponse>('/numbers')});
  return (
    <Layout
      height="fill"
      contentWidth={880}
      header={
        <LayoutHeader>
          <VStack gap={0.5}>
            <Heading level={1}>Phone</Heading>
            <Text type="supporting">What each number does with an incoming call. Every call that nobody answers ends in voicemail, and every call is on the caller’s thread.</Text>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent padding={4}>
          {q.isPending ? (
            <Spinner />
          ) : q.error || !q.data ? (
            <Banner status="error" title="Phone numbers could not be loaded." />
          ) : q.data.numbers.length === 0 ? (
            <Banner status="info" title="No phone numbers yet" description="Ask the server operator to connect this workspace’s numbers." />
          ) : (
            <VStack gap={6}>
              {q.data.numbers.map(n => (
                <NumberCard key={n.number} flows={n} available={q.data.available_modes} voices={q.data.voices ?? []} />
              ))}
              <Entrances />
            </VStack>
          )}
        </LayoutContent>
      }
    />
  );
}

function NumberCard({flows, available, voices}: {flows: NumberFlows; available: NumbersResponse['available_modes']; voices: string[]}) {
  const client = useQueryClient();
  const [flow, setFlow] = useState<CallFlow>(flows.active.flow);
  useEffect(() => setFlow(flows.active.flow), [flows.active.flow]);
  const path = `/numbers/${encodeURIComponent(flows.number)}/call-flow`;
  const refresh = () => client.invalidateQueries({queryKey: ['numbers']});
  const save = useMutation({mutationFn: () => api.put(path, flow), onSuccess: refresh});
  const activate = useMutation({mutationFn: () => api.post(`${path}/activate`), onSuccess: refresh});
  const discard = useMutation({mutationFn: () => api.post(`${path}/discard`), onSuccess: refresh});
  const changed = JSON.stringify(flow) !== JSON.stringify(flows.active.flow);
  const can = (mode: CallMode) => mode === 'ivr' || !!available[mode];

  const setMode = (mode: string) => {
    const m = mode as CallMode;
    setFlow(f => ({
      ...f,
      answer: m === 'forward' ? {mode: m, forward_to: f.answer.forward_to ?? '', then: f.answer.then} : RINGS.includes(m) ? {mode: m, then: f.answer.then} : {mode: m},
      agent: m === 'ai' ? (f.agent ?? {opening: '', instructions: '', voice: 'marin'}) : f.agent,
      menu: m === 'ivr' ? (f.menu ?? {prompt: '', options: [{digit: '1', label: '', action: {mode: 'voicemail'}}], no_choice: {mode: 'voicemail'}}) : undefined,
    }));
  };

  return (
    <Card>
      <VStack gap={4}>
        <HStack gap={3} vAlign="center" wrap="wrap">
          <StackItem size="fill">
            <VStack gap={0.5}>
              <Heading level={2}>{formatNumber(flows.number)}</Heading>
              <Text type="supporting">Now: {describe(flows.active.flow)}</Text>
            </VStack>
          </StackItem>
        </HStack>

        {flows.pending && (
          <Banner
            status="warning"
            title="An agent proposed a change"
            description={`${describe(flows.pending.flow)}. Calls keep using the current setting until you activate it.`}
            endContent={
              <HStack gap={2}>
                <Button label="Activate" variant="primary" size="sm" isLoading={activate.isPending} onClick={() => activate.mutate()} />
                <Button label="Discard" size="sm" isLoading={discard.isPending} onClick={() => discard.mutate()} />
              </HStack>
            }
          />
        )}

        <RadioList label="When someone calls" value={flow.answer.mode} onChange={setMode}>
          {MODES.map(m => (
            <RadioListItem key={m.mode} value={m.mode} label={m.label} description={can(m.mode) ? m.description : `${m.description} Coming soon.`} isDisabled={!can(m.mode)} />
          ))}
        </RadioList>

        {flow.answer.mode === 'forward' && (
          <TextInput label="Forward to" value={flow.answer.forward_to ?? ''} placeholder="+13105550100" onChange={v => setFlow(f => ({...f, answer: {...f.answer, forward_to: v}}))} />
        )}
        {RINGS.includes(flow.answer.mode) && <NextSteps action={flow.answer} depth={1} can={can} onChange={answer => setFlow(f => ({...f, answer}))} />}

        {flow.answer.mode === 'ivr' && flow.menu && <MenuEditor flow={flow} setFlow={setFlow} can={can} />}

        {usesAI(flow) && <AgentEditor agent={flow.agent ?? {opening: '', instructions: '', voice: 'marin'}} voices={voices} onChange={agent => setFlow(f => ({...f, agent}))} />}

        <TextArea label="Voicemail greeting" description="Read to callers before the beep." value={flow.greeting} rows={2} maxLength={500} onChange={v => setFlow(f => ({...f, greeting: v}))} />

        {save.error && <Banner status="error" title={save.error.message} />}
        <HStack gap={2}>
          <Button label="Save" variant="primary" isDisabled={!changed} isLoading={save.isPending} onClick={() => save.mutate()} />
          {changed && <Button label="Undo changes" onClick={() => setFlow(flows.active.flow)} />}
        </HStack>
      </VStack>
    </Card>
  );
}

export function AgentEditor({
  agent,
  voices,
  onChange,
  rulesLabel = 'Rules for calls',
  rulesDescription = 'What it must never promise, what to ask, when to take a message. The phone manner is built in.',
}: {
  agent: CallAgent;
  voices: string[];
  onChange: (a: CallAgent) => void;
  rulesLabel?: string;
  rulesDescription?: string;
}) {
  return (
    <VStack gap={3}>
      <Heading level={3}>The AI</Heading>
      <Text type="supporting">
        It answers from the workspace’s approved info and nothing else. When it can’t help, it takes a message, which notifies you like a voicemail.
      </Text>
      <TextInput label="Opening line" description="Spoken as it picks up." value={agent.opening} placeholder="Thanks for calling. How can I help?" onChange={v => onChange({...agent, opening: v})} />
      <TextArea
        label={rulesLabel}
        description={rulesDescription}
        value={agent.instructions}
        rows={8}
        maxLength={20000}
        onChange={v => onChange({...agent, instructions: v})}
      />
      <HStack gap={2} wrap="wrap" vAlign="end">
        <Selector label="Voice" value={agent.voice || 'marin'} options={voices.map(v => ({value: v, label: v}))} onChange={v => onChange({...agent, voice: v})} />
        <TextInput label="Transfer urgent callers to" description="Optional. Empty: it takes a message." value={agent.transfer_to ?? ''} placeholder="+13105550100" onChange={v => onChange({...agent, transfer_to: v})} />
      </HStack>
    </VStack>
  );
}

export function ActionPicker({label: name, action, onChange, can}: {label: string; action: CallAction; onChange: (a: CallAction) => void; can: (m: CallMode) => boolean}) {
  return (
    <VStack gap={2}>
      <HStack gap={2} wrap="wrap" vAlign="end">
        <Selector
          label={name}
          value={action.mode}
          options={OPTION_MODES.map(m => ({value: m, label: label(m), disabled: !can(m)}))}
          onChange={v => {
            const m = v as CallMode;
            onChange(m === 'forward' ? {mode: m, forward_to: action.forward_to ?? '', then: action.then} : RINGS.includes(m) ? {mode: m, then: action.then} : {mode: m});
          }}
        />
        {action.mode === 'forward' && <TextInput label="Number" value={action.forward_to ?? ''} placeholder="+13105550100" onChange={v => onChange({...action, forward_to: v})} />}
      </HStack>
      {RINGS.includes(action.mode) && <NextSteps action={action} depth={1} can={can} onChange={onChange} />}
    </VStack>
  );
}

// The fallback chain: what happens when a ring or forward goes unanswered
// (or nobody is online to ring). No next step means the caller leaves a
// message.
function NextSteps({action, depth, can, onChange}: {action: CallAction; depth: number; can: (m: CallMode) => boolean; onChange: (a: CallAction) => void}) {
  const next = action.then;
  const choices = [{value: 'message', label: 'Take a message'}, ...OPTION_MODES.filter(m => m !== 'voicemail').map(m => ({value: m, label: label(m), disabled: !can(m)}))];
  const setNext = (then?: CallAction) => onChange({...action, then});
  return (
    <VStack gap={2}>
      <HStack gap={2} wrap="wrap" vAlign="end">
        <Selector
          label={action.mode === 'ring_app' ? 'If nobody answers in the app, or nobody is online' : 'If nobody answers'}
          value={next ? next.mode : 'message'}
          options={choices}
          onChange={v => setNext(v === 'message' ? undefined : v === 'forward' ? {mode: 'forward', forward_to: next?.forward_to ?? ''} : {mode: v as CallMode})}
        />
        {next?.mode === 'forward' && <TextInput label="Number" value={next.forward_to ?? ''} placeholder="+13105550100" onChange={v => setNext({...next, forward_to: v})} />}
      </HStack>
      {next && RINGS.includes(next.mode) && depth + 1 < MAX_STEPS && <NextSteps action={next} depth={depth + 1} can={can} onChange={setNext} />}
    </VStack>
  );
}

function MenuEditor({flow, setFlow, can}: {flow: CallFlow; setFlow: (f: (f: CallFlow) => CallFlow) => void; can: (m: CallMode) => boolean}) {
  const menu = flow.menu!;
  const setMenu = (change: Partial<typeof menu>) => setFlow(f => ({...f, menu: {...f.menu!, ...change}}));
  const used = new Set(menu.options.map(o => o.digit));
  const nextDigit = ['1', '2', '3', '4', '5', '6', '7', '8', '9', '0'].find(d => !used.has(d));
  return (
    <VStack gap={3}>
      <TextArea label="Menu prompt" description="Read to callers, e.g. “For leasing press 1. For current residents press 2.”" value={menu.prompt} rows={2} maxLength={500} onChange={v => setMenu({prompt: v})} />
      {menu.options.map((o, i) => (
        <HStack key={i} gap={2} wrap="wrap" vAlign="end">
          <TextInput label="Key" value={o.digit} width={64} onChange={v => setMenu({options: menu.options.map((x, j) => (j === i ? {...x, digit: v.slice(-1)} : x))})} />
          <TextInput label="Name" value={o.label} placeholder="Leasing" onChange={v => setMenu({options: menu.options.map((x, j) => (j === i ? {...x, label: v} : x))})} />
          <ActionPicker label="Then" action={o.action} can={can} onChange={a => setMenu({options: menu.options.map((x, j) => (j === i ? {...x, action: a} : x))})} />
          <Button label="Remove" variant="ghost" isDisabled={menu.options.length === 1} onClick={() => setMenu({options: menu.options.filter((_, j) => j !== i)})} />
        </HStack>
      ))}
      {nextDigit && menu.options.length < 9 && (
        <Button label="Add an option" onClick={() => setMenu({options: [...menu.options, {digit: nextDigit, label: '', action: {mode: 'voicemail'}}]})} />
      )}
      <ActionPicker label="If they press nothing" action={menu.no_choice} can={can} onChange={a => setMenu({no_choice: a})} />
    </VStack>
  );
}
