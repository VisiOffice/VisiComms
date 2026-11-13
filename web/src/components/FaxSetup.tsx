import {useState} from 'react';
import {useMutation, useQuery} from '@tanstack/react-query';
import {VStack, HStack} from '@astryxdesign/core/Layout';
import {Text, Heading} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {TextInput} from '@astryxdesign/core/TextInput';
import {FileInput} from '@astryxdesign/core/FileInput';
import {Spinner} from '@astryxdesign/core/Spinner';
import {api, ApiError, type FaxSettings, type FaxResult, type FaxDocument} from '../api';
import {CopyLine} from './SetupChecklist';

export function FaxSetup() {
  const query = useQuery({queryKey: ['fax'], queryFn: () => api.get<FaxSettings>('/fax')});
  if (query.isPending) return <Spinner />;
  if (query.error || !query.data) return <Banner status="error" title="Fax settings could not be loaded." />;
  const settings = query.data;
  if (!settings.available) return <Text>The server operator must configure SignalWire before faxes can be sent or received.</Text>;
  return (
    <VStack gap={3}>
      {settings.numbers.length === 0 ? <Text>No fax-capable number is assigned. Ask the server operator to connect one.</Text> : (
        <>
          {settings.numbers.map(n => <Text type="code" key={n.number}>{n.number}</Text>)}
          <FaxComposer key={settings.numbers[0].number} from={settings.numbers[0].number} />
        </>
      )}
      <CopyLine label="Configure the number’s Compatibility fax webhook (POST)" value={settings.inbound_url} />
    </VStack>
  );
}

export function FaxComposer({from: initialFrom, to: fixedTo, onQueued}: {from: string; to?: string; onQueued?: () => Promise<unknown>}) {
  const [from, setFrom] = useState(initialFrom);
  const [to, setTo] = useState(fixedTo ?? '');
  const [requestId, setRequestId] = useState(() => crypto.randomUUID());
  const [file, setFile] = useState<File | null>(null);
  const [result, setResult] = useState<FaxResult | null>(null);
  const [uploaded, setUploaded] = useState<{file: File; doc: FaxDocument} | null>(null);
  const send = useMutation({
    mutationFn: async () => {
      if (!file) throw new Error('Choose a PDF first.');
      let doc = uploaded?.file === file ? uploaded.doc : null;
      if (!doc) {
        const response = await fetch('/api/fax/documents', {
          method: 'POST', credentials: 'same-origin', body: file,
          headers: {'Content-Type': 'application/pdf', 'X-Filename': encodeURIComponent(file.name)},
        });
        const data = await response.json();
        if (!response.ok) throw new ApiError(response.status, data.error ?? 'PDF upload failed.');
        doc = data as FaxDocument;
        setUploaded({file, doc});
      }
      return api.post<FaxResult>('/faxes', {from, to, document_id: doc.id, request_id: requestId});
    },
    onSuccess: async value => {setResult(value); setRequestId(crypto.randomUUID()); setFile(null); setUploaded(null); await onQueued?.();},
  });
  return (
    <VStack gap={3}>
      <Heading level={3}>{fixedTo ? 'Reply by fax' : 'Send a fax'}</Heading>
      <HStack gap={2} wrap="wrap">
        {fixedTo ? <Text>From {from} to {to}</Text> : (
          <>
            <TextInput label="From number" isDisabled={send.isPending} value={from} onChange={setFrom} />
            <TextInput label="To number" isDisabled={send.isPending} value={to} onChange={setTo} placeholder="+14155550123" />
          </>
        )}
      </HStack>
      <FileInput label="PDF document" value={file} onChange={value => {setRequestId(crypto.randomUUID()); setFile(Array.isArray(value) ? value[0] ?? null : value); setResult(null); send.reset();}} accept="application/pdf,.pdf" maxSize={10 * 1024 * 1024} mode="dropzone" isDisabled={send.isPending} description="PDF only, up to 10 MB. This exact document will be transmitted." />
      {send.error && <Banner status="error" title={send.error.message} />}
      <HStack>
        <Button label="Send fax" variant="primary" isLoading={send.isPending} isDisabled={!file || !from || !to} onClick={() => send.mutate()} />
      </HStack>
      <Text type="supporting">Fax transmission may take several minutes. Carrier charges may apply.</Text>
      {result && <Button label="Open fax conversation" href={`/threads/${result.thread_id}`} />}
    </VStack>
  );
}
