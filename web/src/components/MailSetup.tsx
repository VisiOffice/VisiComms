import {useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {Spinner} from '@astryxdesign/core/Spinner';
import {StatusDot} from '@astryxdesign/core/StatusDot';
import {TextInput} from '@astryxdesign/core/TextInput';
import {Token} from '@astryxdesign/core/Token';
import {useToast} from '@astryxdesign/core/Toast';

import {api, type MailSettings, type MailStatus, type PostalAddress} from '../api';
import {CopyLine} from './SetupChecklist';

export function dollars(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`;
}

// Mail: letters printed and posted through the workspace's own Lob account.
export function MailSetup() {
  const q = useQuery({queryKey: ['mail'], queryFn: () => api.get<MailStatus>('/mail')});
  const [editing, setEditing] = useState(false);
  if (q.isPending) return <Spinner />;
  if (q.error || !q.data) return <Banner status="error" title="Mail settings could not be loaded." />;
  const m = q.data;
  if (!m.available) return <Text>The server operator must set VISICOMMS_MASTER_KEY and PUBLIC_URL before Lob can be connected.</Text>;
  if (!m.connected || !m.settings || editing) return <MailForm status={m} onDone={() => setEditing(false)} />;
  const s = m.settings;
  return (
    <VStack gap={3}>
      <Card>
        <HStack gap={3} vAlign="center" wrap="wrap">
          <StatusDot variant={s.health === 'failing' ? 'error' : 'success'} label={s.health === 'failing' ? 'Failing' : 'Connected'} />
          <StackItem size="fill">
            <VStack gap={0.5}>
              <HStack gap={2} vAlign="center">
                <Text weight="semibold">Lob</Text>
                {s.mode === 'test' ? (
                  <Token color="orange" size="sm" label="Test key" description="Lob renders letters but never prints or mails them." />
                ) : (
                  <Token color="green" size="sm" label="Live" description="Letters are printed and mailed, and Lob charges for them." />
                )}
              </HStack>
              <Text type="supporting">Return address: {s.return_address.name}, {s.from}</Text>
              <Text type="supporting">
                {dollars(m.month_spent_cents ?? 0)} of {dollars(s.monthly_cap_cents)} used this month (estimated)
              </Text>
              {s.health === 'failing' && s.last_error && <Text type="supporting">{s.last_error}</Text>}
            </VStack>
          </StackItem>
          <Button label="Change" variant="ghost" size="sm" onClick={() => setEditing(true)} />
        </HStack>
      </Card>
      {s.mode === 'test' && <Banner status="warning" title="Test key: nothing is mailed" description="Letters show on threads as sent, but Lob never prints them. Change to a live key to mail for real." />}
      {s.has_webhook_secret ? (
        <CopyLine label="In Lob, Webhooks, add this URL for the letter events (Live, or Test for a test key)" value={s.webhook_url} />
      ) : (
        <Banner
          status="info"
          title="Letters won't show tracking yet"
          description="Add a webhook in Lob for the letter events, pointing at the URL below, then paste its secret here under Change."
        />
      )}
      {!s.has_webhook_secret && <CopyLine label="Webhook URL" value={s.webhook_url} />}
    </VStack>
  );
}

const emptyAddress: PostalAddress = {name: '', company: '', line1: '', line2: '', city: '', state: '', zip: ''};

function MailForm({status, onDone}: {status: MailStatus; onDone: () => void}) {
  const client = useQueryClient();
  const toast = useToast();
  const current = status.settings;
  const [key, setKey] = useState('');
  const [secret, setSecret] = useState('');
  const [address, setAddress] = useState<PostalAddress>(current?.return_address ?? emptyAddress);
  const [cap, setCap] = useState(String(((current?.monthly_cap_cents ?? status.default_cap_cents ?? 5000) / 100).toFixed(0)));
  const set = (k: keyof PostalAddress) => (v: string) => setAddress(a => ({...a, [k]: v}));
  const save = useMutation({
    mutationFn: () =>
      api.put<MailSettings>('/mail', {api_key: key, webhook_secret: secret, return_address: address, monthly_cap_cents: Math.round(Number(cap) * 100)}),
    onSuccess: async s => {
      toast({body: s.mode === 'test' ? 'Lob connected with a test key. Nothing will be mailed.' : 'Lob connected.'});
      await client.invalidateQueries({queryKey: ['mail']});
      onDone();
    },
  });
  const keep = current ? 'Leave empty to keep the saved one.' : undefined;
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Heading level={3}>{current ? 'Change Lob' : 'Connect Lob'}</Heading>
        <Text type="supporting">
          Letters are printed and mailed by your own Lob account, and Lob bills you for them. Use the secret API key from Lob’s Settings, API Keys: a test key renders
          letters without mailing them.
        </Text>
        <TextInput label="Secret API key" description={keep ?? 'Starts with live_ or test_.'} type="password" value={key} onChange={setKey} />
        <TextInput
          label="Webhook secret"
          description={keep ?? 'Optional now. From the webhook you add in Lob for letter events; without it, letters show no tracking.'}
          type="password"
          value={secret}
          onChange={setSecret}
        />
        <Heading level={3}>Return address</Heading>
        <HStack gap={3} wrap="wrap">
          <TextInput label="Name" value={address.name} onChange={set('name')} />
          <TextInput label="Company (optional)" value={address.company ?? ''} onChange={set('company')} />
        </HStack>
        <HStack gap={3} wrap="wrap">
          <TextInput label="Street" value={address.line1} onChange={set('line1')} />
          <TextInput label="Suite or unit (optional)" value={address.line2 ?? ''} onChange={set('line2')} />
        </HStack>
        <HStack gap={3} wrap="wrap">
          <TextInput label="City" value={address.city} onChange={set('city')} />
          <TextInput label="State" description="Two letters, such as AZ" value={address.state} onChange={set('state')} />
          <TextInput label="ZIP code" value={address.zip} onChange={set('zip')} />
        </HStack>
        <TextInput
          label="Monthly limit, in dollars"
          description="Letters that would pass it are refused. Counted from VisiComms’ estimates; Lob’s bill is final."
          value={cap}
          onChange={setCap}
        />
        {save.error && <Banner status="error" title={(save.error as Error).message} />}
        <HStack gap={2}>
          <Button label={current ? 'Save' : 'Connect'} variant="primary" isLoading={save.isPending} onClick={() => save.mutate()} />
          {current && <Button label="Cancel" variant="ghost" onClick={onDone} />}
        </HStack>
      </VStack>
    </Card>
  );
}
