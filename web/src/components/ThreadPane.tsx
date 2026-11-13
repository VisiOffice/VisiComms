import {useRef, useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Button} from '@astryxdesign/core/Button';
import {IconButton} from '@astryxdesign/core/IconButton';
import {Icon} from '@astryxdesign/core/Icon';
import {Banner} from '@astryxdesign/core/Banner';
import {Card} from '@astryxdesign/core/Card';
import {Section} from '@astryxdesign/core/Section';
import {Token} from '@astryxdesign/core/Token';
import {Divider} from '@astryxdesign/core/Divider';
import {Spinner} from '@astryxdesign/core/Spinner';
import {TextArea} from '@astryxdesign/core/TextArea';
import {TextInput} from '@astryxdesign/core/TextInput';
import {Timestamp} from '@astryxdesign/core/Timestamp';
import {Toolbar} from '@astryxdesign/core/Toolbar';
import {MetadataList, MetadataListItem} from '@astryxdesign/core/MetadataList';
import {useToast} from '@astryxdesign/core/Toast';
import {
  ArrowLeftIcon,
  ArrowUturnLeftIcon,
  ChatBubbleLeftIcon,
  CheckCircleIcon,
  PhoneArrowUpRightIcon,
  PaperAirplaneIcon,
  ExclamationTriangleIcon,
  HandRaisedIcon,
  XCircleIcon,
  XMarkIcon,
} from '@heroicons/react/24/outline';

import {api, ApiError, type Draft, type SetupStatus, type SMSSettings, type Thread, type TimelineEntry} from '../api';
import {FaxComposer} from './FaxSetup';
import {LetterCard, letterSummary, MailingAddresses} from './Letters';
import {dialOut} from './Softphone';
import {useMe} from '../App';

export function useThread(id: string) {
  // A chat is live: re-read it every few seconds for new messages and the
  // visitor's typing. Other channels are read when something is done.
  return useQuery({
    queryKey: ['thread', id],
    queryFn: () => api.get<Thread>(`/threads/${id}`),
    refetchInterval: q => (q.state.data?.channel === 'chat' ? 3000 : false),
  });
}

type Props = {
  threadId: string;
  onClose: () => void;
  /** True when the pane has the whole screen rather than sitting beside the queue. */
  isFullWidth: boolean;
};

export function ThreadPane({threadId, onClose, isFullWidth}: Props) {
  const thread = useThread(threadId);
  const queryClient = useQueryClient();
  const toast = useToast();
  const me = useMe();

  const refresh = () =>
    Promise.all([
      queryClient.invalidateQueries({queryKey: ['thread', threadId]}),
      queryClient.invalidateQueries({queryKey: ['attention']}),
    ]);
  const fail = (err: unknown) =>
    toast({body: err instanceof ApiError ? err.message : 'Something went wrong. Nothing was sent.', type: 'error'});

  const takeOver = useMutation({
    mutationFn: (handBack: boolean) => api.post(`/threads/${threadId}/${handBack ? 'hand-back' : 'take-over'}`),
    onSuccess: (_d, handBack) => {
      toast({body: handBack ? 'Handed back to the agent.' : 'You have this thread. Agents will not send until you hand it back.'});
      return refresh();
    },
    onError: fail,
  });
  const markDone = useMutation({
    mutationFn: (reopen: boolean) => api.post(`/threads/${threadId}/${reopen ? 'reopen' : 'resolve'}`),
    onSuccess: async (_d, reopen) => {
      toast({body: reopen ? 'Reopened. It is back on Needs attention.' : 'Marked done. A new message or call from them reopens it.'});
      await refresh();
      if (!reopen) onClose();
    },
    onError: fail,
  });

  if (thread.isPending) {
    return (
      <VStack padding={6} hAlign="center">
        <Spinner />
      </VStack>
    );
  }
  if (thread.error) {
    return (
      <VStack padding={6}>
        <Banner status="error" title="This thread could not be loaded." description={thread.error.message} />
      </VStack>
    );
  }

  const t = thread.data;
  const email = t.contact.identities?.find(i => i.channel === t.channel);
  const address = t.channel === 'chat' ? 'the website visitor' : ['sms', 'fax'].includes(t.channel) ? t.remote_address : email?.address;
  const optedOut = (t.contact.opted_out ?? []).includes(t.channel);
  const isTakenOver = Boolean(t.taken_over_by);
  const isMine = isTakenOver && t.taken_over_by_id === me.data?.person.id;

  const heading = (
    <Heading level={2} maxLines={2}>
      {t.channel === 'postal'
        ? `Letters · ${t.contact.name || t.remote_address}`
        : ['sms', 'fax'].includes(t.channel)
          ? `${t.channel.toUpperCase()} · ${address ?? t.subject}`
          : t.subject || '(no subject)'}
    </Heading>
  );
  const takeOverButton = (
    <Button
      label={isTakenOver ? 'Hand back' : 'Take over'}
      variant="secondary"
      size="sm"
      icon={<Icon icon={HandRaisedIcon} size="sm" />}
      isLoading={takeOver.isPending}
      onClick={() => takeOver.mutate(isTakenOver)}
    />
  );
  const isDone = t.state === 'resolved';
  const actions = (
    <HStack gap={2}>
      <Button
        label={isDone ? 'Reopen' : 'Mark done'}
        variant={isDone ? 'secondary' : 'primary'}
        size="sm"
        icon={<Icon icon={isDone ? ArrowUturnLeftIcon : CheckCircleIcon} size="sm" />}
        isLoading={markDone.isPending}
        onClick={() => markDone.mutate(isDone)}
      />
      {takeOverButton}
    </HStack>
  );

  return (
    <VStack height="100%">
      {isFullWidth ? (
        <>
          <Toolbar
            label="Thread"
            startContent={<Button label="Needs attention" variant="ghost" icon={<Icon icon={ArrowLeftIcon} size="sm" />} onClick={onClose} />}
            endContent={actions}
          />
          <VStack paddingBlockEnd={3}>{heading}</VStack>
        </>
      ) : (
        <VStack paddingBlockStart={4} paddingInlineStart={3} paddingInlineEnd={2}>
          <Toolbar
            label="Thread"
            startContent={heading}
            endContent={
              <>
                {actions}
                <IconButton variant="ghost" label="Close thread" tooltip="Close" icon={<Icon icon={XMarkIcon} size="sm" />} onClick={onClose} />
              </>
            }
          />
        </VStack>
      )}

      <StackItem size="fill">
        <VStack paddingInline={isFullWidth ? 0 : 6} paddingBlockEnd={isFullWidth ? 0 : 6} gap={5} isScrollable height="100%">
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Text weight="semibold">{t.contact.name || address}</Text>
            {address && <Text color="secondary">{address}</Text>}
            <Token size="sm" label={t.channel === 'email' ? 'Email' : t.channel === 'chat' ? 'Website chat' : t.channel.toUpperCase()} />
            <Token size="sm" color={t.state === 'open' ? 'blue' : t.state === 'waiting' ? 'gray' : 'green'} label={STATE_LABEL[t.state]} />
          </HStack>

          {isTakenOver && (
            <Banner
              status="info"
              title={isMine ? 'You have this thread' : `${t.taken_over_by} has taken over this thread`}
              description={
                isMine
                  ? 'Agents can read and draft here, but nothing they propose is sent until you hand it back. Write your reply below.'
                  : 'Agents can read and draft here, but nothing they propose is sent until it is handed back.'
              }
            />
          )}
          {optedOut && <Banner status="warning" title={`This contact opted out of ${t.channel === 'sms' ? 'SMS' : t.channel}`} description="Replies on this channel are blocked for people and agents." />}

          {(t.proposals ?? []).map(d => (
            <DraftCard key={d.id} draft={d} isBlocked={optedOut} onDone={refresh} onError={fail} />
          ))}

          <VStack gap={3}>
            <Heading level={3}>Timeline</Heading>
            <VStack gap={4}>
              {(t.timeline ?? []).map(e => (
                <TimelineItem key={e.id} entry={e} contact={t.contact.name} threadId={threadId} />
              ))}
              {t.visitor_typing && <Text color="secondary">{t.contact.name || 'The visitor'} is typing…</Text>}
            </VStack>
          </VStack>

          {!optedOut && (!isTakenOver || isMine) && (
            // Keyed on holding the thread: taking over remounts it open, ready to type.
            t.channel === 'fax' && t.phone_number && t.remote_address ? (
              <FaxComposer key={t.id} from={t.phone_number} to={t.remote_address} onQueued={refresh} />
            ) : t.channel === 'postal' ? (
              <MailingAddresses key={t.id} contactId={t.contact.id} onQueued={refresh} />
            ) : t.channel === 'voice' ? (
              t.phone_number && t.remote_address?.startsWith('+') ? (
                <CallerActions key={t.id} threadId={threadId} from={t.phone_number} to={t.remote_address} onDone={refresh} onError={fail} />
              ) : null
            ) : <ReplyBox key={isMine ? 'mine' : 'open'} threadId={threadId} subject={t.subject} to={address} channel={t.channel} from={t.phone_number} startOpen={isMine} onSent={refresh} onError={fail} />
          )}

          <Divider />

          <MetadataList title={<Heading level={3}>Contact</Heading>}>
            <MetadataListItem label="Name">{t.contact.name || 'Unknown'}</MetadataListItem>
            {(t.contact.identities ?? []).map(i => (
              <MetadataListItem key={i.channel + i.address} label={i.channel === 'email' ? 'Email' : i.channel}>
                {i.address} · {i.verified ? 'verified' : 'not verified'}
              </MetadataListItem>
            ))}
            <MetadataListItem label="Opted out">{(t.contact.opted_out ?? []).join(', ') || 'No'}</MetadataListItem>
            <MetadataListItem label="Trusted">{t.contact.trusted ? 'Yes' : 'No'}</MetadataListItem>
            {t.contact.tags.length > 0 && (
              <MetadataListItem label="Tags">
                <HStack gap={1} wrap="wrap">
                  {t.contact.tags.map(tag => (
                    <Token key={tag} size="sm" label={tag} />
                  ))}
                </HStack>
              </MetadataListItem>
            )}
          </MetadataList>

          {t.channel !== 'postal' && <MailingAddresses key={t.contact.id} contactId={t.contact.id} onQueued={refresh} />}

          {(t.links ?? []).length > 0 && (
            <MetadataList title={<Heading level={3}>Linked records</Heading>}>
              {(t.links ?? []).map(l => (
                <MetadataListItem key={l.system + l.record_type + l.external_id} label={`${l.system} ${l.record_type.replace('_', ' ')}`}>
                  {l.external_id} · {l.state || 'no state'}
                  {l.pushed_at && (
                    <>
                      {' '}
                      · pushed <Timestamp value={l.pushed_at} format="relative" />
                    </>
                  )}
                </MetadataListItem>
              ))}
            </MetadataList>
          )}

          {(t.contact.memory ?? []).length > 0 && (
            <VStack gap={2}>
              <Heading level={3}>Agent memory</Heading>
              {(t.contact.memory ?? []).map((m, i) => (
                <VStack key={i} gap={0.5}>
                  <Text>{m.text}</Text>
                  <Text type="supporting">{m.source}</Text>
                </VStack>
              ))}
            </VStack>
          )}
        </VStack>
      </StackItem>
    </VStack>
  );
}

const STATE_LABEL = {open: 'Open', waiting: 'Waiting on customer', resolved: 'Resolved'} as const;

// A person's own reply. It goes through the same checks as an approved draft
// and out through the executor; a held agent draft on the thread is voided.
// A call is answered by texting or calling the caller back, from the number
// they called. The text goes to their SMS conversation, where any answer
// lands; the call leaves Needs attention either way.
function CallerActions({
  threadId,
  from,
  to,
  onDone,
  onError,
}: {
  threadId: string;
  from: string;
  to: string;
  onDone: () => Promise<unknown>;
  onError: (err: unknown) => void;
}) {
  const toast = useToast();
  const sms = useQuery({queryKey: ['sms'], queryFn: () => api.get<SMSSettings>('/sms')});
  const canText = Boolean(sms.data?.available && sms.data.numbers.some(n => n.number === from));
  const [isOpen, setIsOpen] = useState(false);
  const [body, setBody] = useState('');
  const length = Array.from(body).length;
  const text = useMutation({
    mutationFn: () => api.post<{state: string; sms_thread_id: string}>(`/threads/${threadId}/text-back`, {body}),
    onSuccess: res => {
      toast({body: res.state === 'delivered' ? `Texted ${to}.` : 'Text queued for sending.'});
      setBody('');
      setIsOpen(false);
      return onDone();
    },
    onError,
  });
  const call = useMutation({
    mutationFn: async () => {
      const numbers = await api.post<{from: string; to: string}>(`/threads/${threadId}/call-back`, {});
      await dialOut(numbers.to, numbers.from);
    },
    onSuccess: () => {
      toast({body: `Calling ${to} from ${from}.`});
      return onDone();
    },
    onError,
  });

  if (!isOpen) {
    return (
      <HStack gap={2} wrap="wrap">
        <Button label="Text back" variant="primary" icon={<Icon icon={ChatBubbleLeftIcon} size="sm" />} onClick={() => setIsOpen(true)} />
        <Button label="Call back" icon={<Icon icon={PhoneArrowUpRightIcon} size="sm" />} isLoading={call.isPending} onClick={() => call.mutate()} />
      </HStack>
    );
  }
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Heading level={3}>Text back</Heading>
        {sms.isSuccess && !canText && (
          <Banner status="warning" title={`${from} can't send texts yet`} description="Connect it for SMS (visicomms sms connect) once it is on an approved 10DLC campaign." />
        )}
        <MetadataList>
          <MetadataListItem label="To">{to}</MetadataListItem>
          <MetadataListItem label="From">{from}</MetadataListItem>
        </MetadataList>
        <Text type="supporting">{length} / 1600 characters. Carrier segment charges may apply. Their answer arrives in a text conversation.</Text>
        <TextArea label="Message" value={body} onChange={setBody} rows={6} hasAutoFocus />
        <HStack gap={2} wrap="wrap">
          <Button
            label="Send text"
            variant="primary"
            icon={<Icon icon={PaperAirplaneIcon} size="sm" />}
            isDisabled={body.trim() === '' || !canText || length > 1600}
            isLoading={text.isPending}
            onClick={() => text.mutate()}
          />
          <Button label="Cancel" variant="ghost" onClick={() => setIsOpen(false)} />
        </HStack>
      </VStack>
    </Card>
  );
}

function ReplyBox({
  threadId,
  channel,
  from,
  subject,
  to,
  startOpen,
  onSent,
  onError,
}: {
  threadId: string;
  channel: string;
  from?: string;
  subject: string;
  to?: string;
  startOpen: boolean;
  onSent: () => Promise<unknown>;
  onError: (err: unknown) => void;
}) {
  const toast = useToast();
  const setup = useQuery({queryKey: ['setup'], queryFn: () => api.get<SetupStatus>('/setup')});
  const sms = useQuery({queryKey: ['sms'], queryFn: () => api.get<SMSSettings>('/sms'), enabled: channel === 'sms'});
  const sending = channel === 'chat' ? {state: 'done'} : channel === 'sms' ? {state: sms.data?.available && sms.data.numbers.some(n => n.number === from) ? 'done' : 'todo'} : setup.data?.steps.find(s => s.key === 'sending');
  const [isOpen, setIsOpen] = useState(startOpen);
  const [replySubject, setReplySubject] = useState(subject.toLowerCase().startsWith('re:') ? subject : `Re: ${subject}`);
  const [body, setBody] = useState('');
  const [buttons, setButtons] = useState('');
  const typingSent = useRef(0);
  const write = (text: string) => {
    setBody(text);
    // The visitor sees "… is typing"; tell them at most every few seconds.
    if (channel === 'chat' && text.trim() !== '' && Date.now() - typingSent.current > 3000) {
      typingSent.current = Date.now();
      api.post(`/threads/${threadId}/typing`).catch(() => undefined);
    }
  };
  const quickReplies = buttons.split(',').map(b => b.trim()).filter(Boolean);
  const send = useMutation({
    mutationFn: () => api.post<{state: string}>(`/threads/${threadId}/reply`, {subject: replySubject, body, quick_replies: channel === 'chat' ? quickReplies : undefined}),
    onSuccess: res => {
      toast({body: res.state === 'delivered' ? (channel === 'chat' ? 'Sent to the chat.' : `Sent to ${to}.`) : 'Queued for sending.'});
      setBody('');
      setButtons('');
      setIsOpen(false);
      return onSent();
    },
    onError,
  });

  if (!to) {
    return null;
  }
  if (!isOpen) {
    return (
      <HStack>
        <Button label="Reply" variant="primary" icon={<Icon icon={ArrowUturnLeftIcon} size="sm" />} onClick={() => setIsOpen(true)} />
      </HStack>
    );
  }
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Heading level={3}>Reply</Heading>
        {sending?.state === 'todo' ? (
          <Banner
            status="warning"
            title="This workspace can't send yet"
            description={channel === 'sms' ? 'Connect your SignalWire number in Connections.' : 'Connect sending, or use the sandbox for now, from the setup checklist on Needs attention.'}
            endContent={<Button label="Open checklist" href="/" size="sm" />}
          />
        ) : sending?.state === 'partial' ? (
          <Text type="supporting">Sandbox: your reply is recorded as sent but nobody receives it until real sending is connected.</Text>
        ) : null}
        <MetadataList>
          <MetadataListItem label="To">{to}</MetadataListItem>
        </MetadataList>
        {channel === 'email' && <TextInput label="Subject" value={replySubject} onChange={setReplySubject} />}
        {channel === 'sms' && <Text type="supporting">From {from} · {Array.from(body).length} / 1600 characters. Carrier segment charges may apply.</Text>}
        {channel === 'chat' && <Text type="supporting">Appears in the visitor's chat window under your first name · {Array.from(body).length} / 4000 characters.</Text>}
        <TextArea label="Message" value={body} onChange={write} rows={8} hasAutoFocus />
        {channel === 'chat' && (
          <TextInput
            label="Buttons (optional)"
            description="Up to 4 quick replies the visitor can tap, separated by commas, such as Yes, No."
            value={buttons}
            onChange={setButtons}
          />
        )}
        <HStack gap={2} wrap="wrap">
          <Button
            label="Send"
            variant="primary"
            icon={<Icon icon={PaperAirplaneIcon} size="sm" />}
            isDisabled={body.trim() === '' || sending?.state === 'todo' || (channel === 'sms' && Array.from(body).length > 1600) || (channel === 'chat' && (Array.from(body).length > 4000 || quickReplies.length > 4 || quickReplies.some(b => Array.from(b).length > 40)))}
            isLoading={send.isPending}
            onClick={() => send.mutate()}
          />
          <Button label="Cancel" variant="ghost" onClick={() => setIsOpen(false)} />
        </HStack>
      </VStack>
    </Card>
  );
}

function DraftCard({
  draft,
  isBlocked,
  onDone,
  onError,
}: {
  draft: Draft;
  isBlocked: boolean;
  onDone: () => Promise<unknown>;
  onError: (err: unknown) => void;
}) {
  const toast = useToast();
  const [mode, setMode] = useState<'view' | 'edit' | 'reject'>('view');
  const [subject, setSubject] = useState(draft.payload.subject ?? '');
  const [body, setBody] = useState(draft.payload.body);
  const [note, setNote] = useState('');
  const to = draft.recipients.map(r => r.email ?? r.phone).join(', ');

  const approve = useMutation({
    mutationFn: (edit: boolean) => api.post(`/proposals/${draft.id}/approve`, edit ? {edit: {subject, body}} : {}),
    onSuccess: () => {
      toast({body: `Sent to ${to}.`});
      return onDone();
    },
    onError,
  });
  const reject = useMutation({
    mutationFn: () => api.post(`/proposals/${draft.id}/reject`, {note}),
    onSuccess: () => {
      toast({body: 'Rejected. Your note goes back to the agent.'});
      return onDone();
    },
    onError,
  });

  return (
    <Card padding={0}>
      <Section variant="muted" padding={4} dividers={['bottom']}>
        <HStack gap={2} vAlign="center" wrap="wrap">
          <StackItem size="fill">
            <VStack gap={0.5}>
              <Text weight="semibold">Reply held for you</Text>
              <Text type="supporting">
                Drafted by {draft.agent} · <Timestamp value={draft.created_at} format="relative" />
              </Text>
            </VStack>
          </StackItem>
          <Token color="orange" label={`Held: ${draft.reason || draft.rule}`} />
        </HStack>
      </Section>

      <VStack padding={4} gap={4}>
        <MetadataList>
          <MetadataListItem label="To">{to}</MetadataListItem>
          {mode !== 'edit' && draft.action === 'email.send' && <MetadataListItem label="Subject">{draft.payload.subject}</MetadataListItem>}
        </MetadataList>

        {mode === 'edit' ? (
          <VStack gap={3}>
            {draft.action === 'email.send' && <TextInput label="Subject" value={subject} onChange={setSubject} />}
            <TextArea label="Message" value={body} onChange={setBody} rows={10} />
          </VStack>
        ) : (
          <VStack gap={2}>
            <Text as="p" style={{whiteSpace: 'pre-wrap'}}>
              {draft.payload.body}
            </Text>
            <QuickReplyTokens replies={draft.payload.quick_replies} />
          </VStack>
        )}

        {draft.checks.length > 0 && (
          <VStack gap={2}>
            <Text type="label">Checked when drafted</Text>
            {draft.checks.map(c => (
              <HStack key={c.label} gap={2} vAlign="center">
                <Icon icon={c.ok ? CheckCircleIcon : ExclamationTriangleIcon} size="sm" color={c.ok ? 'success' : 'warning'} />
                <Text>{c.label}</Text>
              </HStack>
            ))}
          </VStack>
        )}

        {mode === 'reject' && (
          <TextArea
            label="Note for the agent"
            description="Say what was wrong. The agent sees this and drafts again."
            value={note}
            onChange={setNote}
            rows={3}
            hasAutoFocus
          />
        )}

        <HStack gap={2} wrap="wrap">
          {mode === 'view' && (
            <>
              <Button label="Approve and send" variant="primary" isDisabled={isBlocked} isLoading={approve.isPending} onClick={() => approve.mutate(false)} />
              <Button label="Edit, then approve" isDisabled={isBlocked} onClick={() => setMode('edit')} />
              <Button label="Reject with note" icon={<Icon icon={XCircleIcon} size="sm" />} onClick={() => setMode('reject')} />
            </>
          )}
          {mode === 'edit' && (
            <>
              <Button label="Approve edited reply" variant="primary" isLoading={approve.isPending} onClick={() => approve.mutate(true)} />
              <Button
                label="Cancel"
                variant="ghost"
                onClick={() => {
                  setSubject(draft.payload.subject ?? '');
                  setBody(draft.payload.body);
                  setMode('view');
                }}
              />
            </>
          )}
          {mode === 'reject' && (
            <>
              <Button label="Reject" variant="destructive" isDisabled={note.trim() === ''} isLoading={reject.isPending} onClick={() => reject.mutate()} />
              <Button label="Cancel" variant="ghost" onClick={() => setMode('view')} />
            </>
          )}
        </HStack>

        <Text type="supporting">
          Approval covers this exact text, this recipient
          {draft.approved_info_version ? ` and approved info v${draft.approved_info_version}` : ''}. A change to any of them voids it, and the send is
          checked again just before it goes out.
        </Text>
      </VStack>
    </Card>
  );
}

// The recording streams through VisiComms with the session cookie; the
// provider link never reaches the browser. Loaded only when played.
// Recordings are deleted a year after the call (internal/recordings).
const RECORDING_DAYS = 365;

function RecordingPlayer({callID, at}: {callID: string; at: string}) {
  const [failed, setFailed] = useState(false);
  if (Date.now() - new Date(at).getTime() >= RECORDING_DAYS * 86_400_000) {
    return <Text color="secondary">Recording deleted a year after the call.</Text>;
  }
  if (failed) {
    return <Text color="secondary">The recording could not be loaded.</Text>;
  }
  return <audio controls preload="none" src={`/api/calls/${encodeURIComponent(callID)}/recording`} onError={() => setFailed(true)} style={{width: '100%'}} />;
}

function QuickReplyTokens({replies}: {replies?: string[]}) {
  if (!replies || replies.length === 0) return null;
  return (
    <HStack gap={1} wrap="wrap">
      {replies.map(r => (
        <Token key={r} size="sm" color="blue" label={r} description="A button the visitor can tap" />
      ))}
    </HStack>
  );
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

function TimelineItem({entry, contact, threadId}: {entry: TimelineEntry; contact: string; threadId: string}) {
  const p = entry.payload;
  const when = <Timestamp value={entry.at} format="auto" />;

  switch (entry.type) {
    case 'postal.queued':
    case 'postal.sent':
      return <LetterCard entry={entry} threadId={threadId} />;
    case 'fax.received':
    case 'fax.queued':
    case 'fax.sent':
      return (
        <VStack gap={2}>
          <Text weight="semibold">{entry.type === 'fax.received' ? 'Fax received' : entry.type === 'fax.queued' ? 'Fax queued' : 'Fax accepted by SignalWire'}</Text>
          {when}
          {Number(p.pages) > 0 && <Text>{Number(p.pages)} pages</Text>}
          {str(p.document_id) && <Button label={str(p.filename) || 'Download PDF'} href={`/api/fax/documents/${str(p.document_id)}`} />}
        </VStack>
      );
    case 'sms.received':
    case 'chat.received':
    case 'email.received': {
      const visitor = (p.visitor ?? {}) as Record<string, unknown>;
      const typed = ['name', 'email', 'phone'].map(k => str(visitor[k])).filter(Boolean);
      return (
        <VStack gap={2}>
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Text weight="semibold">{contact || 'Customer'} wrote</Text>
            <Token size="sm" color="gray" label="Untrusted" description="Written by the customer. Agents treat it as data, not instructions." />
            {when}
          </HStack>
          {entry.suspicious && (
            <Banner status="warning" title="This message may try to instruct an agent" description="Sends on this thread are held for a person." />
          )}
          <Card variant="muted" padding={3}>
            <Text as="p" style={{whiteSpace: 'pre-wrap'}}>
              {str(p.text)}
            </Text>
            {Number(p.media_count) > 0 && <Text type="supporting">This SMS includes media; downloads are not supported yet.</Text>}
          </Card>
          {entry.type === 'chat.received' && str(p.page) && <Text type="supporting">On {str(p.page)}</Text>}
          {typed.length > 0 && <Text type="supporting">Typed into the chat, not verified: {typed.join(' · ')}</Text>}
        </VStack>
      );
    }
    case 'call.ai_handled': {
      const lines = Array.isArray(p.transcript) ? (p.transcript as {speaker: string; text: string}[]) : [];
      return (
        <VStack gap={2}>
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Text weight="semibold">The AI answered {contact || 'the caller'}</Text>
            <Token size="sm" color="gray" label="Untrusted" description="Includes the caller’s words. Agents treat it as data, not instructions." />
            {when}
          </HStack>
          {entry.suspicious && (
            <Banner status="warning" title="The caller may have tried to instruct an agent" description="Sends on this thread are held for a person." />
          )}
          <Card variant="muted" padding={3}>
            <Text as="p" style={{whiteSpace: 'pre-wrap'}}>
              {str(p.summary)}
            </Text>
          </Card>
          {lines.length > 0 && (
            <VStack gap={1}>
              {lines.map((l, i) => (
                <Text key={i} type="supporting">
                  {l.speaker === 'ai' ? 'AI' : contact || 'Caller'}: {l.text}
                </Text>
              ))}
            </VStack>
          )}
        </VStack>
      );
    }
    case 'call.texted_back':
      return (
        <VStack gap={2}>
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Text weight="semibold">{entry.actor || 'We'} texted back from {str(p.from)}</Text>
            {when}
          </HStack>
          <Card padding={3}>
            <Text as="p" style={{whiteSpace: 'pre-wrap'}}>
              {str(p.text)}
            </Text>
          </Card>
          {str(p.sms_thread_id) && (
            <HStack>
              <Button label="Open the text conversation" size="sm" variant="ghost" href={`/threads/${str(p.sms_thread_id)}`} />
            </HStack>
          )}
        </VStack>
      );
    case 'call.recorded':
    case 'voice.voicemail': {
      const seconds = Number(p.duration_seconds) || 0;
      const length = seconds > 0 ? ` · ${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}` : '';
      return (
        <VStack gap={2}>
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Text weight="semibold">
              {entry.type === 'voice.voicemail' ? `${contact || 'The caller'} left a voicemail` : 'Call recording'}
              {length}
            </Text>
            {when}
          </HStack>
          {str(p.call_id) && <RecordingPlayer callID={str(p.call_id)} at={entry.at} />}
        </VStack>
      );
    }
    case 'chat.sent':
      if (str(p.popup_id)) {
        return (
          <VStack gap={2}>
            <HStack gap={2} vAlign="center" wrap="wrap">
              <Text weight="semibold">Pop-up message shown as {str(p.sender)}</Text>
              {when}
            </HStack>
            <Card variant="muted" padding={3}>
              <Text as="p" style={{whiteSpace: 'pre-wrap'}}>
                {str(p.text)}
              </Text>
            </Card>
          </VStack>
        );
      }
      return (
        <VStack gap={2}>
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Text weight="semibold">{entry.actor || 'We'} replied</Text>
            {when}
          </HStack>
          <Card padding={3}>
            <VStack gap={2}>
              <Text as="p" style={{whiteSpace: 'pre-wrap'}}>
                {str(p.text)}
              </Text>
              <QuickReplyTokens replies={p.quick_replies as string[] | undefined} />
            </VStack>
          </Card>
        </VStack>
      );
    case 'sms.queued':
    case 'sms.sent':
    case 'email.queued':
    case 'email.sent':
      return (
        <VStack gap={2}>
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Text weight="semibold">{entry.actor || 'We'} replied</Text>
            {p.sandbox === true && <Token size="sm" color="purple" label="Sandbox: not really sent" />}
            {entry.type.endsWith('.queued') && <Token size="sm" color="orange" label="Queued: not sent yet" />}
            {when}
          </HStack>
          <Card padding={3}>
            <Text as="p" style={{whiteSpace: 'pre-wrap'}}>
              {str(p.text)}
            </Text>
          </Card>
        </VStack>
      );
    default:
      return (
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Text color="secondary">{eventLine(entry)}</Text>
          {when}
        </HStack>
      );
  }
}

function eventLine(e: TimelineEntry): string {
  const p = e.payload;
  const who = e.actor || 'Someone';
  switch (e.type) {
    case 'proposal.held':
      return `${who} drafted a reply. Held: ${str(p.reason) || str(p.rule)}.`;
    case 'proposal.approved':
      return `${who} approved the reply${p.edited ? ' after editing it' : ''}${p.via_agent ? `, through an agent: “${str(p.person_said)}”` : ''}.`;
    case 'proposal.rejected':
      return `${who} rejected the draft${p.via_agent ? ' through an agent' : ''}: “${str(p.note)}”`;
    case 'proposal.voided':
      return `A held draft was voided: ${str(p.reason)}.`;
    case 'triage.needs_person':
      return `Triage sent this to a person: ${str(p.note)}. ${str(p.detail)}`;
    case 'run.failed':
      return `The agent run failed: ${str(p.reason)}`;
    case 'fax.send_unknown':
    case 'sms.send_unknown':
    case 'email.send_unknown':
      return `A send could not be confirmed. ${str(p.note)}`;
    case 'fax.delivered':
      return `Fax delivered${Number(p.pages) > 0 ? ` · ${Number(p.pages)} pages` : ''}.`;
    case 'fax.failed':
    case 'fax.receive_failed':
      return `Fax failed: ${str(p.reason) || 'the transmission did not complete'}`;
    case 'fax.delivery_update':
      return 'SignalWire updated the fax transmission status.';
    case 'sms.delivered':
      return 'The carrier reported the SMS delivered.';
    case 'sms.delivery_update':
      return 'SignalWire updated the SMS delivery status.';
    case 'email.delivered':
      return 'Delivered to the customer’s mail server.';
    case 'email.bounced':
      return `Bounced: ${str(p.reason) || 'the address does not accept mail'}. No more email goes to this contact.`;
    case 'email.complained':
      return 'The customer marked a reply as spam. They are opted out of email.';
    case 'email.delayed':
      return `Delivery delayed${str(p.reason) ? `: ${str(p.reason)}` : ''}. Amazon keeps retrying.`;
    case 'sms.failed':
      return `SMS delivery failure reported: ${str(p.reason)}`;
    case 'email.failed':
      return `Not sent: ${str(p.reason)}`;
    case 'fax.cancelled':
    case 'sms.cancelled':
    case 'email.cancelled':
      return `Not sent: ${str(p.reason)}`;
    case 'call.called_back':
      return `${who} called back from ${str(p.from)}.`;
    case 'call.received':
      return `Call from ${str(p.from)} to ${str(p.to)}.`;
    case 'thread.taken_over':
      return `${who} took over the thread.`;
    case 'thread.handed_back':
      return `${who} handed the thread back to the agent.`;
    case 'thread.resolved':
      return `${who} marked the thread done.`;
    case 'thread.claimed':
      return `${who} is working on a reply.`;
    case 'thread.released':
      return `${who} stopped working on it.`;
    case 'thread.reopened':
      return `${who} reopened the thread.`;
    default:
      return letterSummary(e) ?? e.type;
  }
}
