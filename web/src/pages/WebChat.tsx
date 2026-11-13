import {useEffect, useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {Layout, LayoutContent, LayoutHeader, HStack, VStack} from '@astryxdesign/core/Layout';
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

import {api, type ChatWidget, type ChatWidgets} from '../api';
import {CopyLine} from '../components/SetupChecklist';
import {ChatPopups} from '../components/ChatPopups';

const ASK = [
  {value: 'off', label: 'Don’t ask'},
  {value: 'optional', label: 'Optional'},
  {value: 'required', label: 'Required'},
];

export function WebChatPage() {
  const q = useQuery({queryKey: ['chat-widgets'], queryFn: () => api.get<ChatWidgets>('/chat/widgets')});
  return (
    <Layout
      height="fill"
      contentWidth={880}
      header={
        <LayoutHeader>
          <VStack gap={0.5}>
            <Heading level={1}>Web chat</Heading>
            <Text type="supporting">
              A chat window on your own website. Each chat is a thread in Needs attention, answered like an email or a text. Nothing is saved until a visitor sends a message.
            </Text>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent padding={4}>
          {q.isPending ? (
            <Spinner />
          ) : q.error || !q.data ? (
            <Banner status="error" title="Web chat settings could not be loaded." />
          ) : (
            <VStack gap={4}>
              {q.data.widgets.map(w => (
                <WidgetCard key={w.id} widget={w} scriptURL={q.data.script_url} />
              ))}
              <NewWidget first={q.data.widgets.length === 0} />
            </VStack>
          )}
        </LayoutContent>
      }
    />
  );
}

function NewWidget({first}: {first: boolean}) {
  const client = useQueryClient();
  const [name, setName] = useState('');
  const [error, setError] = useState('');
  const create = useMutation({
    mutationFn: () => api.post<ChatWidget>('/chat/widgets', {name}),
    onSuccess: () => {
      setName('');
      setError('');
      return client.invalidateQueries({queryKey: ['chat-widgets']});
    },
    onError: (e: Error) => setError(e.message),
  });
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Heading level={2}>{first ? 'Add a chat to your website' : 'Add another chat'}</Heading>
        <Text type="supporting">One per website. The name is what visitors see at the top of the chat window, and on replies an agent sends.</Text>
        <HStack gap={2} vAlign="end" wrap="wrap">
          <TextInput label="Name" placeholder="Your business" value={name} onChange={setName} status={error ? {type: 'error', message: error} : undefined} />
          <Button label="Add chat" variant="primary" isDisabled={name.trim() === ''} isLoading={create.isPending} onClick={() => create.mutate()} />
        </HStack>
      </VStack>
    </Card>
  );
}

function snippet(scriptURL: string, publicID: string) {
  return `<script>window.visicomms=window.visicomms||function(){(visicomms.q=visicomms.q||[]).push(arguments)}</script>
<script src="${scriptURL}" data-widget="${publicID}" async></script>`;
}

function WidgetCard({widget, scriptURL}: {widget: ChatWidget; scriptURL: string}) {
  const client = useQueryClient();
  const toast = useToast();
  const [s, setS] = useState(widget);
  const [origins, setOrigins] = useState(widget.allowed_origins.join('\n'));
  const [buttons, setButtons] = useState(widget.quick_replies.join(', '));
  const [error, setError] = useState<{field?: string; message: string} | null>(null);
  useEffect(() => {
    setS(widget);
    setOrigins(widget.allowed_origins.join('\n'));
    setButtons(widget.quick_replies.join(', '));
  }, [widget]);
  const save = useMutation({
    mutationFn: () => api.put<ChatWidget>(`/chat/widgets/${widget.id}`, {...s, allowed_origins: origins.split('\n'), quick_replies: buttons.split(',').map(b => b.trim()).filter(Boolean)}),
    onSuccess: () => {
      setError(null);
      toast({body: 'Chat settings saved.'});
      return client.invalidateQueries({queryKey: ['chat-widgets']});
    },
    onError: (e: Error & {field?: string}) => setError({message: e.message}),
  });
  const set = <K extends keyof ChatWidget>(key: K) => (value: ChatWidget[K]) => setS(prev => ({...prev, [key]: value}));
  const changed =
    JSON.stringify(s) !== JSON.stringify(widget) || origins !== widget.allowed_origins.join('\n') || buttons !== widget.quick_replies.join(', ');
  return (
    <Card padding={4}>
      <VStack gap={4}>
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Heading level={2}>{widget.name}</Heading>
          {widget.disabled ? <Token size="sm" color="gray" label="Off" /> : <Token size="sm" color="green" label="On" />}
        </HStack>
        <CopyLine label="Paste this before </body> on every page of the site" value={snippet(scriptURL, widget.public_id)} />
        <Text type="supporting">
          Your site can open the chat from its own buttons with visicomms('open'), and pass a signed-in customer’s details with visicomms('setUser', {'{'}name, email, phone{'}'}). Details passed this way are still treated as unverified.
        </Text>

        <Heading level={3}>Look and wording</Heading>
        <HStack gap={3} wrap="wrap">
          <TextInput label="Name" value={s.name} onChange={set('name')} />
          <TextInput label="Colour" description="A hex colour, such as #2563eb" value={s.color} onChange={set('color')} />
          <Selector
            label="Position"
            value={s.position}
            options={[
              {value: 'right', label: 'Bottom right'},
              {value: 'left', label: 'Bottom left'},
            ]}
            onChange={v => set('position')(v as 'right' | 'left')}
          />
        </HStack>
        <TextInput label="Greeting" value={s.greeting} onChange={set('greeting')} />
        <TextInput label="Greeting for returning visitors" value={s.returning_greeting} onChange={set('returning_greeting')} />
        <TextInput
          label="Buttons under the greeting (optional)"
          description="Up to 4 quick replies a new visitor can tap to start, separated by commas, such as Book a visit, Opening hours."
          value={buttons}
          onChange={setButtons}
        />
        <TextInput label="Reply time" description="Shown under the name during business hours." value={s.reply_time} onChange={set('reply_time')} />
        <TextArea
          label="Away message"
          description="Shown outside your business hours, with a field for the visitor’s email so the reply can reach them."
          value={s.offline_message}
          onChange={set('offline_message')}
          rows={3}
        />

        <Heading level={3}>Before the first message, ask for</Heading>
        <HStack gap={3} wrap="wrap">
          <Selector label="Name" value={s.ask_name} options={ASK} onChange={v => set('ask_name')(v as ChatWidget['ask_name'])} />
          <Selector label="Email" value={s.ask_email} options={ASK} onChange={v => set('ask_email')(v as ChatWidget['ask_email'])} />
          <Selector label="Phone" value={s.ask_phone} options={ASK} onChange={v => set('ask_phone')(v as ChatWidget['ask_phone'])} />
        </HStack>
        <Text type="supporting">What visitors type is not verified, so it is shown on the thread but never added to the contact as a way to reach them.</Text>

        <Heading level={3}>Sites</Heading>
        <TextArea
          label="Sites that may show this chat"
          description="One per line, such as https://www.example.com. Leave empty to allow any site. This stops the chat appearing on another site by accident; it is not a security control."
          value={origins}
          onChange={setOrigins}
          rows={3}
        />
        <CheckboxInput label="Turn this chat off" description="The chat disappears from the site. Past conversations stay in the inbox." value={s.disabled} onChange={set('disabled')} />

        {error && <Banner status="error" title={error.message} />}
        <HStack gap={2}>
          <Button label="Save" variant="primary" isDisabled={!changed} isLoading={save.isPending} onClick={() => save.mutate()} />
          {changed && (
            <Button
              label="Discard changes"
              variant="ghost"
              onClick={() => {
                setS(widget);
                setOrigins(widget.allowed_origins.join('\n'));
                setButtons(widget.quick_replies.join(', '));
              }}
            />
          )}
        </HStack>
        <ChatPopups widget={widget} />
      </VStack>
    </Card>
  );
}
