import {useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {Icon} from '@astryxdesign/core/Icon';
import {Spinner} from '@astryxdesign/core/Spinner';
import {StatusDot} from '@astryxdesign/core/StatusDot';
import {TextInput} from '@astryxdesign/core/TextInput';
import {Timestamp} from '@astryxdesign/core/Timestamp';
import {Token} from '@astryxdesign/core/Token';
import {useToast} from '@astryxdesign/core/Toast';
import {ArrowPathIcon, CheckCircleIcon, ClockIcon} from '@heroicons/react/24/outline';

import {api, ApiError} from '../api';
import {CopyLine} from './SetupChecklist';

type Record = {type: string; name: string; value: string; purpose: string; ok: boolean; priority?: number};
type Domain = {id: string; domain: string; ownership_verified: boolean; provider: string; status: string; verified: boolean; records: Record[]; checked_at?: string};
type SendingStatus = {domains: Domain[]; from: {name: string; address: string} | null; available: boolean; provider: 'ses' | 'resend'};

// Sending as the business's own domain: add the domain, publish the records
// shown, then choose the From address. The server selects SES or Resend.
export function SendingSetup() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const status = useQuery({queryKey: ['sending'], queryFn: () => api.get<SendingStatus>('/sending')});
  const refresh = () => Promise.all(['sending', 'setup', 'connections'].map(k => queryClient.invalidateQueries({queryKey: [k]})));
  const fail = (err: unknown) => toast({body: err instanceof ApiError ? err.message : 'That didn’t work. Try again.', type: 'error'});
  const [domain, setDomain] = useState('');
  const add = useMutation({
    mutationFn: () => api.post<Domain>('/sending/domains', {domain}),
    onSuccess: () => {
      setDomain('');
      return refresh();
    },
    onError: fail,
  });

  if (status.isPending) {
    return <Spinner />;
  }
  if (status.error || !status.data) {
    return <Banner status="error" title="Sending settings could not be loaded." />;
  }
  const s = status.data;
  if (!s.available) {
    return <Banner status="warning" title="Sending isn't configured on this server yet" description="The platform's email provider has no key, so replies can't be sent." />;
  }
  const verified = s.domains.filter(d => d.verified);

  return (
    <VStack gap={3}>
      <Text type="supporting">Replies are sent through {s.provider === 'resend' ? 'Resend' : 'Amazon SES'}.</Text>
      {s.domains.map(d => (
        <DomainCard key={d.id} domain={d} onChecked={refresh} onError={fail} />
      ))}

      <Card>
        <VStack gap={2}>
          <Text weight="semibold">{s.domains.length ? 'Add another domain' : 'Send as your own domain'}</Text>
          <Text type="supporting">The domain your customers already get mail from, like yourbusiness.com. You'll add a few DNS records to prove it's yours.</Text>
          <HStack gap={2} vAlign="end" wrap="wrap">
            <TextInput label="Domain" isLabelHidden placeholder="yourbusiness.com" value={domain} onChange={setDomain} onEnter={() => add.mutate()} width={260} />
            <Button label="Add domain" isLoading={add.isPending} isDisabled={!domain.trim()} onClick={() => add.mutate()} />
          </HStack>
        </VStack>
      </Card>

      <FromForm current={s.from} verifiedDomains={verified.map(d => d.domain)} onSaved={refresh} onError={fail} />
    </VStack>
  );
}

function DomainCard({domain, onChecked, onError}: {domain: Domain; onChecked: () => Promise<unknown>; onError: (e: unknown) => void}) {
  const check = useMutation({mutationFn: () => api.post<Domain>(`/sending/domains/${domain.id}/check`), onSuccess: onChecked, onError});
  return (
    <Card>
      <VStack gap={3}>
        <HStack gap={2} vAlign="center" wrap="wrap">
          <StatusDot variant={domain.verified ? 'success' : 'warning'} label={domain.verified ? 'Verified' : 'Waiting for DNS'} />
          <StackItem size="fill">
            <VStack gap={0.5}>
              <Text weight="semibold">{domain.domain}</Text>
              <Text type="supporting">
                {domain.verified ? 'Verified: replies can be sent as this domain.' : 'Add these records where your domain’s DNS is managed, then check.'}
                {domain.checked_at && (
                  <>
                    {' '}
                    Checked <Timestamp value={domain.checked_at} format="relative" />.
                  </>
                )}
              </Text>
            </VStack>
          </StackItem>
          <Button label="Check now" size="sm" icon={<Icon icon={ArrowPathIcon} size="sm" />} isLoading={check.isPending} onClick={() => check.mutate()} />
        </HStack>
        {domain.records.map(r => (
          <VStack key={`${r.type}:${r.name}`} gap={1}>
            <HStack gap={2} vAlign="center" wrap="wrap">
              <Icon icon={r.ok ? CheckCircleIcon : ClockIcon} size="sm" color={r.ok ? 'success' : 'secondary'} />
              <Token size="sm" label={r.type} />
              <Text type="supporting">{r.purpose}</Text>
            </HStack>
            <CopyLine label="Name" value={r.name} />
            <CopyLine label="Value" value={r.value} />
            {r.priority !== undefined && <CopyLine label="Priority" value={String(r.priority)} />}
          </VStack>
        ))}
      </VStack>
    </Card>
  );
}

function FromForm({
  current,
  verifiedDomains,
  onSaved,
  onError,
}: {
  current: {name: string; address: string} | null;
  verifiedDomains: string[];
  onSaved: () => Promise<unknown>;
  onError: (e: unknown) => void;
}) {
  const toast = useToast();
  const [name, setName] = useState(current?.name ?? '');
  const [address, setAddress] = useState(current?.address ?? (verifiedDomains[0] ? `hello@${verifiedDomains[0]}` : ''));
  const save = useMutation({
    mutationFn: () => api.post('/sending/from', {name, address}),
    onSuccess: () => {
      toast({body: 'Replies will come from ' + address + '.'});
      return onSaved();
    },
    onError,
  });
  if (verifiedDomains.length === 0) {
    return null;
  }
  return (
    <Card>
      <VStack gap={2}>
        <Heading level={3}>Replies come from</Heading>
        {current?.address && (
          <Text>
            {current.name ? `${current.name} <${current.address}>` : current.address}
          </Text>
        )}
        <HStack gap={2} vAlign="end" wrap="wrap">
          <TextInput label="Name" value={name} onChange={setName} placeholder="Your business" width={220} />
          <TextInput label="Address" type="email" value={address} onChange={setAddress} placeholder={`quotes@${verifiedDomains[0]}`} width={260} />
          <Button label="Save" variant="primary" isLoading={save.isPending} isDisabled={!address.trim()} onClick={() => save.mutate()} />
        </HStack>
        <Text type="supporting">Use the address customers already write to, so their replies come back to it and are forwarded here.</Text>
      </VStack>
    </Card>
  );
}
