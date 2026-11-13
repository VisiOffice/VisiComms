import {useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {CheckboxInput} from '@astryxdesign/core/CheckboxInput';
import {FileInput} from '@astryxdesign/core/FileInput';
import {Selector} from '@astryxdesign/core/Selector';
import {Spinner} from '@astryxdesign/core/Spinner';
import {TextInput} from '@astryxdesign/core/TextInput';
import {Timestamp} from '@astryxdesign/core/Timestamp';
import {Token} from '@astryxdesign/core/Token';
import {useToast} from '@astryxdesign/core/Toast';

import {
  api,
  ApiError,
  type ExtraService,
  type FaxDocument,
  type LetterEstimate,
  type LetterOptions,
  type LetterResult,
  type MailStatus,
  type PostalAddress,
  type SavedAddress,
  type TimelineEntry,
} from '../api';
import {dollars} from './MailSetup';

const SERVICES: {value: ExtraService; label: string}[] = [
  {value: '', label: 'First class'},
  {value: 'certified', label: 'Certified mail'},
  {value: 'certified_return_receipt', label: 'Certified, return receipt'},
];

function oneLine(a: PostalAddress): string {
  return [a.line1, a.line2, `${a.city} ${a.state} ${a.zip}`].filter(Boolean).join(', ');
}

function canMail(a: SavedAddress): boolean {
  return !a.returned && a.deliverability !== 'undeliverable';
}

// AddressStatus says what Lob's check found, in words.
function AddressStatus({a}: {a: SavedAddress}) {
  if (a.returned) return <Token size="sm" color="red" label="Mail came back" description="A letter to this address was returned. Save it again once it's corrected." />;
  switch (a.deliverability) {
    case 'deliverable':
      return null;
    case 'unchecked':
      return <Token size="sm" color="gray" label="Not checked" description="Saved with a Lob test key, which can't check addresses." />;
    case 'deliverable_missing_unit':
      return <Token size="sm" color="orange" label="Unit missing" description="USPS knows the building but expects an apartment or suite number." />;
    case 'deliverable_incorrect_unit':
      return <Token size="sm" color="orange" label="Unit not found" description="USPS doesn't know this apartment or suite number." />;
    case 'deliverable_unnecessary_unit':
      return <Token size="sm" color="gray" label="Unit not needed" description="USPS delivers to this address without a unit number." />;
    default:
      return <Token size="sm" color="gray" label={a.deliverability} />;
  }
}

// MailingAddresses lists a contact's mailing addresses and lets a person add
// one or send a letter.
export function MailingAddresses({contactId, startWriting = false, onQueued}: {contactId: string; startWriting?: boolean; onQueued?: () => Promise<unknown>}) {
  const addresses = useQuery({
    queryKey: ['addresses', contactId],
    queryFn: () => api.get<{addresses: SavedAddress[]}>(`/contacts/${contactId}/addresses`),
  });
  const mail = useQuery({queryKey: ['mail'], queryFn: () => api.get<MailStatus>('/mail')});
  const [adding, setAdding] = useState(false);
  const [writing, setWriting] = useState(startWriting);
  if (addresses.isPending || mail.isPending) return <Spinner />;
  if (!mail.data?.available) return null;
  const list = addresses.data?.addresses ?? [];
  const connected = Boolean(mail.data?.connected);
  const mailable = list.filter(canMail);
  return (
    <VStack gap={3}>
      <Heading level={3}>Mailing addresses</Heading>
      {list.length === 0 && <Text type="supporting">No mailing address yet.</Text>}
      {list.map(a => (
        <HStack key={a.id} gap={2} vAlign="center" wrap="wrap">
          <StackItem size="fill">
            <VStack gap={0.5}>
              <Text>{a.name}{a.company ? `, ${a.company}` : ''}</Text>
              <Text type="supporting">{oneLine(a)}</Text>
            </VStack>
          </StackItem>
          <AddressStatus a={a} />
        </HStack>
      ))}
      {!connected && <Text type="supporting">Connect Lob under Connections, Mail, to check addresses and mail letters.</Text>}
      {connected && adding && <AddressForm contactId={contactId} onDone={() => setAdding(false)} />}
      {connected && !adding && !writing && (
        <HStack gap={2}>
          <Button label="Add an address" variant="secondary" size="sm" onClick={() => setAdding(true)} />
          {mailable.length > 0 && <Button label="Send a letter" variant="secondary" size="sm" onClick={() => setWriting(true)} />}
        </HStack>
      )}
      {connected && writing && mailable.length > 0 && mail.data?.settings && (
        <LetterComposer
          contactId={contactId}
          addresses={mailable}
          test={mail.data.settings.mode === 'test'}
          onCancel={() => setWriting(false)}
          onQueued={async () => {
            setWriting(false);
            await onQueued?.();
          }}
        />
      )}
    </VStack>
  );
}

function AddressForm({contactId, onDone}: {contactId: string; onDone: () => void}) {
  const client = useQueryClient();
  const [a, setA] = useState<PostalAddress>({name: '', company: '', line1: '', line2: '', city: '', state: '', zip: ''});
  const set = (k: keyof PostalAddress) => (v: string) => setA(x => ({...x, [k]: v}));
  const save = useMutation({
    mutationFn: () => api.post<SavedAddress>(`/contacts/${contactId}/addresses`, a),
    onSuccess: async () => {
      await client.invalidateQueries({queryKey: ['addresses', contactId]});
      onDone();
    },
  });
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Text type="supporting">Lob checks the address with USPS and saves it the way USPS writes it.</Text>
        <HStack gap={3} wrap="wrap">
          <TextInput label="Name" description="Who the letter is addressed to" value={a.name} onChange={set('name')} />
          <TextInput label="Company (optional)" value={a.company ?? ''} onChange={set('company')} />
        </HStack>
        <HStack gap={3} wrap="wrap">
          <TextInput label="Street" value={a.line1} onChange={set('line1')} />
          <TextInput label="Apartment or suite (optional)" value={a.line2 ?? ''} onChange={set('line2')} />
        </HStack>
        <HStack gap={3} wrap="wrap">
          <TextInput label="City" value={a.city} onChange={set('city')} />
          <TextInput label="State" description="Two letters" value={a.state} onChange={set('state')} />
          <TextInput label="ZIP code" value={a.zip} onChange={set('zip')} />
        </HStack>
        {save.error && <Banner status="error" title={(save.error as Error).message} />}
        <HStack gap={2}>
          <Button label="Check and save" variant="primary" size="sm" isLoading={save.isPending} onClick={() => save.mutate()} />
          <Button label="Cancel" variant="ghost" size="sm" onClick={onDone} />
        </HStack>
      </VStack>
    </Card>
  );
}

async function uploadPDF(file: File): Promise<FaxDocument> {
  const response = await fetch('/api/fax/documents', {
    method: 'POST',
    credentials: 'same-origin',
    body: file,
    headers: {'Content-Type': 'application/pdf', 'X-Filename': encodeURIComponent(file.name)},
  });
  const data = await response.json();
  if (!response.ok) throw new ApiError(response.status, data.error ?? 'PDF upload failed.');
  return data as FaxDocument;
}

function LetterComposer({
  contactId,
  addresses,
  test,
  onCancel,
  onQueued,
}: {
  contactId: string;
  addresses: SavedAddress[];
  test: boolean;
  onCancel: () => void;
  onQueued: () => Promise<unknown>;
}) {
  const toast = useToast();
  const [addressId, setAddressId] = useState(addresses[0].id);
  const [file, setFile] = useState<File | null>(null);
  const [options, setOptions] = useState<LetterOptions>({extra_service: '', color: false, double_sided: true});
  const [requestId, setRequestId] = useState(() => crypto.randomUUID());
  const upload = useMutation({mutationFn: uploadPDF});
  const doc = upload.data;
  const estimate = useQuery({
    queryKey: ['letter-estimate', doc?.id, options],
    queryFn: () => api.post<LetterEstimate>('/letters/estimate', {document_id: doc?.id, ...options}),
    enabled: Boolean(doc),
  });
  const send = useMutation({
    mutationFn: () => api.post<LetterResult>('/letters', {contact_id: contactId, address_id: addressId, document_id: doc?.id, ...options, request_id: requestId}),
    onSuccess: async () => {
      toast({body: test ? 'Letter queued. With a test key, Lob won’t mail it.' : 'Letter queued for Lob.'});
      setRequestId(crypto.randomUUID());
      await onQueued();
    },
  });
  const change = (o: Partial<LetterOptions>) => {
    setOptions(x => ({...x, ...o}));
    setRequestId(crypto.randomUUID());
  };
  const cost = estimate.data ? dollars(estimate.data.cost_cents) : null;
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Heading level={3}>Send a letter</Heading>
        {test && <Banner status="warning" title="Test key: this letter won't be mailed" />}
        <Selector label="To" value={addressId} options={addresses.map(a => ({value: a.id, label: `${a.name}, ${oneLine(a)}`}))} onChange={v => setAddressId(v)} />
        <FileInput
          label="Letter (PDF)"
          value={file}
          onChange={value => {
            const f = Array.isArray(value) ? (value[0] ?? null) : value;
            setFile(f);
            setRequestId(crypto.randomUUID());
            send.reset();
            if (f) upload.mutate(f);
            else upload.reset();
          }}
          accept="application/pdf,.pdf"
          maxSize={10 * 1024 * 1024}
          mode="dropzone"
          isDisabled={send.isPending}
          description="US letter size (8.5 × 11 in), up to 10 MB. Lob adds a blank first page for the address, so leave none."
        />
        <HStack gap={3} wrap="wrap" vAlign="end">
          <Selector label="Mail" value={options.extra_service} options={SERVICES} onChange={v => change({extra_service: v as ExtraService})} />
          <CheckboxInput label="Print on both sides" value={options.double_sided} onChange={v => change({double_sided: v})} />
          <CheckboxInput label="Colour" value={options.color} onChange={v => change({color: v})} />
        </HStack>
        {upload.error && <Banner status="error" title={(upload.error as Error).message} />}
        {estimate.data && (
          <Text type="supporting">
            {estimate.data.pages} {estimate.data.pages === 1 ? 'page' : 'pages'} on {estimate.data.sheets} sheets, including the address page. About {cost}; Lob’s bill
            is final.
          </Text>
        )}
        {send.error && <Banner status="error" title={(send.error as Error).message} />}
        <HStack gap={2}>
          <Button
            label={cost ? `Send letter · ${cost}` : 'Send letter'}
            variant="primary"
            isLoading={send.isPending}
            isDisabled={!doc || !estimate.data || upload.isPending}
            onClick={() => send.mutate()}
          />
          <Button label="Cancel" variant="ghost" onClick={onCancel} />
        </HStack>
        <Text type="supporting">Lob prints and mails it. You can cancel it on the thread only until Lob’s send time, usually minutes away.</Text>
      </VStack>
    </Card>
  );
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

const SERVICE_NAMES: Record<string, string> = {certified: 'Certified mail', certified_return_receipt: 'Certified mail, return receipt'};

// LetterCard is a letter on a thread's timeline, queued or accepted by Lob.
export function LetterCard({entry, threadId}: {entry: TimelineEntry; threadId: string}) {
  const p = entry.payload;
  const client = useQueryClient();
  const toast = useToast();
  const sent = entry.type === 'postal.sent';
  const sendDate = str(p.send_date);
  const cancel = useMutation({
    mutationFn: () => api.post(`/letters/${str(p.action_id)}/cancel`, {}),
    onSuccess: async () => {
      toast({body: 'Letter cancelled. Lob doesn’t charge for it.'});
      await client.invalidateQueries({queryKey: ['thread', threadId]});
    },
    onError: e => toast({body: (e as Error).message}),
  });
  const cancellable = sent && sendDate !== '' && new Date(sendDate).getTime() > Date.now();
  return (
    <VStack gap={2}>
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Text weight="semibold">{sent ? 'Letter accepted by Lob' : 'Letter queued'}</Text>
        {p.test === true && <Token size="sm" color="orange" label="Test, not mailed" description="Sent with a Lob test key. Nothing was printed or mailed." />}
        <Timestamp value={entry.at} format="auto" />
      </HStack>
      <Text>
        To {str(p.name)}, {str(p.address)}
      </Text>
      <Text type="supporting">
        {[SERVICE_NAMES[str(p.extra_service)] ?? 'First class', Number(p.pages) > 0 ? `${Number(p.pages)} pages` : '', p.color ? 'colour' : '',
          Number(p.cost_cents) > 0 ? `about ${dollars(Number(p.cost_cents))}` : '']
          .filter(Boolean)
          .join(' · ')}
      </Text>
      {sent && str(p.expected_delivery_date) && <Text type="supporting">Expected delivery {str(p.expected_delivery_date)}</Text>}
      {str(p.tracking_number) && <Text type="supporting">Tracking {str(p.tracking_number)}</Text>}
      <HStack gap={2} wrap="wrap">
        {str(p.document_id) && <Button label={str(p.filename) || 'Download PDF'} size="sm" href={`/api/fax/documents/${str(p.document_id)}`} />}
        {sent && str(p.url) && <Button label="Lob's proof" size="sm" variant="secondary" href={str(p.url)} />}
        {cancellable && <Button label="Cancel letter" size="sm" variant="ghost" isLoading={cancel.isPending} onClick={() => cancel.mutate()} />}
      </HStack>
      {cancellable && (
        <Text type="supporting">
          Can be cancelled until <Timestamp value={sendDate} format="auto" />.
        </Text>
      )}
    </VStack>
  );
}

// letterSummary words a letter's later events for the timeline.
export function letterSummary(e: TimelineEntry): string | null {
  const p = e.payload;
  switch (e.type) {
    case 'postal.rendered':
      return 'Lob rendered the letter for printing.';
    case 'postal.mailed':
      return 'The letter was handed to USPS.';
    case 'postal.in_transit': {
      const scan = str(p.scan);
      return scan === 'in_local_area' ? 'The letter reached the local post office.' : scan === 're-routed' ? 'USPS re-routed the letter.' : 'USPS scanned the letter in transit.';
    }
    case 'postal.processed_for_delivery':
      return 'USPS processed the letter for delivery, usually the last scan for first class mail.';
    case 'postal.delivered':
      return 'USPS delivered the letter.';
    case 'postal.returned':
      return 'The letter was returned to sender. No more letters go to that address until it is saved again.';
    case 'postal.notice':
      return str(p.scan) === 'pickup_available' ? 'The certified letter is waiting at the post office for pickup.' : 'USPS reported a problem delivering the certified letter.';
    case 'postal.failed':
      return `The letter failed: ${str(p.reason) || 'Lob could not produce it'}.`;
    case 'postal.cancelled':
      return `Letter not sent: ${str(p.reason) || 'cancelled'}`;
    case 'postal.send_unknown':
      return `Lob did not confirm the letter. ${str(p.note)}`;
    default:
      return null;
  }
}
