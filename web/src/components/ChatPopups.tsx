import {useEffect, useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {HStack, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {CheckboxInput} from '@astryxdesign/core/CheckboxInput';
import {Selector} from '@astryxdesign/core/Selector';
import {Spinner} from '@astryxdesign/core/Spinner';
import {TextArea} from '@astryxdesign/core/TextArea';
import {TextInput} from '@astryxdesign/core/TextInput';
import {Token} from '@astryxdesign/core/Token';
import {useToast} from '@astryxdesign/core/Toast';

import {api, type ChatPopup, type ChatPopupSettings, type ChatPopupTrigger, type ChatWidget} from '../api';

const TRIGGERS: {value: ChatPopupTrigger; label: string}[] = [
  {value: 'delay', label: 'After some time on the page'},
  {value: 'page', label: 'On a particular page'},
  {value: 'exit', label: 'When they move to leave (desktop)'},
  {value: 'scroll', label: 'After scrolling down the page'},
  {value: 'idle', label: 'When they stop moving for a while'},
];

const BLANK: ChatPopupSettings = {
  position: 0,
  name: '',
  text: '',
  sender_name: '',
  trigger: 'delay',
  seconds: 20,
  scroll_percent: 50,
  page_pattern: '',
  opens_window: false,
  sound: false,
  devices: 'all',
  frequency: 'session',
  only_if_none: true,
  quick_replies: [],
  active: true,
};

function describe(p: ChatPopupSettings): string {
  switch (p.trigger) {
    case 'delay':
      return `After ${p.seconds} seconds on any page`;
    case 'page':
      return `On ${p.page_pattern}${p.seconds ? `, after ${p.seconds} seconds` : ''}`;
    case 'exit':
      return 'When they move to leave';
    case 'scroll':
      return `After scrolling ${p.scroll_percent}% down`;
    case 'idle':
      return `After ${p.seconds} seconds without moving`;
  }
}

// Pop-up messages for one widget: a short list with how each is doing, and
// a form to add or change one. Kept inside the widget's card on Web chat.
export function ChatPopups({widget}: {widget: ChatWidget}) {
  const q = useQuery({queryKey: ['chat-popups', widget.id], queryFn: () => api.get<{popups: ChatPopup[]}>(`/chat/widgets/${widget.id}/popups`)});
  const [editing, setEditing] = useState<ChatPopup | 'new' | null>(null);
  const popups = q.data?.popups ?? [];
  return (
    <VStack gap={3}>
      <Heading level={3}>Pop-up messages</Heading>
      <Text type="supporting">
        A message that appears to visitors who pine’t written yet, to start the conversation. Nothing about the visitor is saved when one shows; if they reply, it becomes the
        first line of the chat. Each shows at most once per visit, and none shows once a visitor has a conversation.
      </Text>
      {q.isPending ? (
        <Spinner />
      ) : q.error ? (
        <Banner status="error" title="Pop-ups could not be loaded." />
      ) : (
        popups.map(p =>
          editing !== 'new' && editing?.id === p.id ? (
            <PopupForm key={p.id} widget={widget} popup={p} onDone={() => setEditing(null)} />
          ) : (
            <Card key={p.id} variant="muted" padding={3}>
              <VStack gap={1}>
                <HStack gap={2} vAlign="center" wrap="wrap">
                  <Text weight="semibold">{p.name}</Text>
                  {p.active ? <Token size="sm" color="green" label="On" /> : <Token size="sm" color="gray" label="Off" />}
                  <Text type="supporting">
                    {describe(p)} · shown {p.shown}, {p.chats} {p.chats === 1 ? 'chat' : 'chats'} in 30 days
                    {p.shown > 0 ? ` (${Math.round((p.chats / p.shown) * 100)}%)` : ''}
                  </Text>
                </HStack>
                <Text>{p.text}</Text>
                <HStack>
                  <Button label="Edit" size="sm" variant="ghost" onClick={() => setEditing(p)} />
                </HStack>
              </VStack>
            </Card>
          ),
        )
      )}
      {editing === 'new' ? (
        <PopupForm widget={widget} position={popups.length} onDone={() => setEditing(null)} />
      ) : (
        <HStack>
          <Button label="Add a pop-up" onClick={() => setEditing('new')} />
        </HStack>
      )}
    </VStack>
  );
}

function PopupForm({widget, popup, position = 0, onDone}: {widget: ChatWidget; popup?: ChatPopup; position?: number; onDone: () => void}) {
  const client = useQueryClient();
  const toast = useToast();
  const [s, setS] = useState<ChatPopupSettings>(popup ?? {...BLANK, position});
  const [buttons, setButtons] = useState((popup?.quick_replies ?? []).join(', '));
  const [error, setError] = useState('');
  useEffect(() => setError(''), [s]);
  const set = <K extends keyof ChatPopupSettings>(key: K) => (value: ChatPopupSettings[K]) => setS(prev => ({...prev, [key]: value}));
  const done = () => {
    onDone();
    return client.invalidateQueries({queryKey: ['chat-popups', widget.id]});
  };
  const save = useMutation({
    mutationFn: () => {
      const body = {...s, quick_replies: buttons.split(',').map(b => b.trim()).filter(Boolean)};
      return popup ? api.put<ChatPopup>(`/chat/popups/${popup.id}`, body) : api.post<ChatPopup>(`/chat/widgets/${widget.id}/popups`, body);
    },
    onSuccess: () => {
      toast({body: 'Pop-up saved.'});
      return done();
    },
    onError: (e: Error) => setError(e.message),
  });
  const remove = useMutation({
    mutationFn: () => api.delete(`/chat/popups/${popup!.id}`),
    onSuccess: () => {
      toast({body: 'Pop-up deleted.'});
      return done();
    },
    onError: (e: Error) => setError(e.message),
  });
  const number = (v: string) => Math.max(0, Math.round(Number(v) || 0));
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Heading level={4}>{popup ? `Edit ${popup.name}` : 'New pop-up'}</Heading>
        <TextInput label="Name" description="For your own list; visitors don’t see it." placeholder="Services page nudge" value={s.name} onChange={set('name')} />
        <TextArea label="Message" value={s.text} onChange={set('text')} rows={3} placeholder="Questions about a repair? Ask us here." />
        <TextInput label="From" description={`Leave empty to show it as from ${widget.name}.`} value={s.sender_name} onChange={set('sender_name')} />
        <TextInput
          label="Buttons (optional)"
          description="Up to 4 quick replies the visitor can tap, separated by commas, such as Book a visit, Opening hours."
          value={buttons}
          onChange={setButtons}
        />

        <Selector label="When it shows" value={s.trigger} options={TRIGGERS} onChange={v => set('trigger')(v as ChatPopupTrigger)} />
        <HStack gap={3} wrap="wrap">
          {s.trigger === 'page' && (
            <TextInput label="Page" description="A path, with * for any part: /services or /repairs/*" value={s.page_pattern} onChange={set('page_pattern')} />
          )}
          {(s.trigger === 'delay' || s.trigger === 'page' || s.trigger === 'idle') && (
            <TextInput
              label={s.trigger === 'idle' ? 'Seconds without moving' : 'Seconds on the page'}
              value={String(s.seconds)}
              onChange={v => set('seconds')(number(v))}
            />
          )}
          {s.trigger === 'scroll' && <TextInput label="Percent of the page" value={String(s.scroll_percent)} onChange={v => set('scroll_percent')(number(v))} />}
          <Selector
            label="Devices"
            value={s.devices}
            options={[
              {value: 'all', label: 'Phones and computers'},
              {value: 'desktop', label: 'Computers only'},
              {value: 'mobile', label: 'Phones only'},
            ]}
            onChange={v => set('devices')(v as ChatPopupSettings['devices'])}
          />
          <Selector
            label="How often"
            value={s.frequency}
            options={[
              {value: 'session', label: 'Once per visit'},
              {value: 'first_visit', label: 'First visit only'},
            ]}
            onChange={v => set('frequency')(v as ChatPopupSettings['frequency'])}
          />
        </HStack>
        <CheckboxInput label="Open the chat window" description="Otherwise it shows as a small bubble beside the chat button." value={s.opens_window} onChange={set('opens_window')} />
        <CheckboxInput label="Play a sound" description="Browsers only allow it after the visitor has clicked on the page." value={s.sound} onChange={set('sound')} />
        <CheckboxInput label="Only if no other pop-up has shown this visit" value={s.only_if_none} onChange={set('only_if_none')} />
        <CheckboxInput label="On" value={s.active} onChange={set('active')} />

        {error && <Banner status="error" title={error} />}
        <HStack gap={2} wrap="wrap">
          <Button label="Save pop-up" variant="primary" isLoading={save.isPending} onClick={() => save.mutate()} />
          <Button label="Cancel" variant="ghost" onClick={onDone} />
          {popup && <Button label="Delete" variant="ghost" isLoading={remove.isPending} onClick={() => remove.mutate()} />}
        </HStack>
      </VStack>
    </Card>
  );
}
