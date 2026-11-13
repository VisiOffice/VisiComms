import {useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {Button} from '@astryxdesign/core/Button';
import {Layout, LayoutContent, LayoutHeader, HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Card} from '@astryxdesign/core/Card';
import {CodeBlock} from '@astryxdesign/core/CodeBlock';
import {Spinner} from '@astryxdesign/core/Spinner';
import {StatusDot} from '@astryxdesign/core/StatusDot';
import {Timestamp} from '@astryxdesign/core/Timestamp';
import {Token} from '@astryxdesign/core/Token';

import {api, type Connections} from '../api';
import {FaxSetup} from '../components/FaxSetup';
import {MailSetup} from '../components/MailSetup';
import {SMSSetup} from '../components/SMSSetup';
import {SendingSetup} from '../components/SendingSetup';
import {SlackSetup} from '../components/SlackSetup';

const HEALTH = {ok: {variant: 'success', label: 'Working'}, failing: {variant: 'error', label: 'Failing'}, unknown: {variant: 'neutral', label: 'Not checked yet'}} as const;
const AUTONOMY = {observe: 'Observe: drafts stay internal', approve: 'Approve: a person releases each send', autonomous: 'Autonomous: acts within its rules'} as const;

const LINKS_EXAMPLE = `PUT /v1/links/{system}/{record_type}/{id}
Authorization: Bearer <workspace API key>

{
  "state": "pending",
  "updated_at": "2026-10-08T16:14:00Z",
  "contact": {"email": "marcus@roofco.example"},
  "thread_id": "optional, when you know it"
}`;

export function ConnectionsPage() {
  const q = useQuery({queryKey: ['connections'], queryFn: () => api.get<Connections>('/connections')});
  const c = q.data;

  return (
    <Layout
      height="fill"
      contentWidth={880}
      header={
        <LayoutHeader>
          <VStack gap={0.5}>
            <Heading level={1}>Connections</Heading>
            <Text type="supporting">Where mail comes in, how it goes out, the systems that tell us about your records, and the agents that act for you.</Text>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent padding={4}>
          {q.isPending ? (
            <Spinner />
          ) : q.error || !c ? (
            <Banner status="error" title="Connections could not be loaded." />
          ) : (
            <VStack gap={6}>
              <Section title="Receiving email" description="Forward your inquiries address here. Every message becomes an event on its thread.">
                {(c.inboxes ?? []).map(address => (
                  <Card key={address}>
                    <VStack gap={1}>
                      <Text type="label">Forward to</Text>
                      <Text type="code">{address}</Text>
                    </VStack>
                  </Card>
                ))}
              </Section>

              <Section title="Sending" description="Replies go out from your own domain through the configured email provider.">
                <SendingSetup />
                {(c.connectors ?? []).filter(k => k.kind !== 'signalwire').map(k => (
                  <Card key={k.id}>
                    <HStack gap={3} vAlign="center" wrap="wrap">
                      <StatusDot variant={HEALTH[k.health].variant} label={HEALTH[k.health].label} />
                      <StackItem size="fill">
                        <VStack gap={0.5}>
                          <Text weight="semibold">{k.name}</Text>
                          <Text type="supporting">
                            {HEALTH[k.health].label}
                            {k.checked_at && (
                              <>
                                {' '}
                                · checked <Timestamp value={k.checked_at} format="relative" />
                              </>
                            )}
                          </Text>
                          {k.last_error && <Text color="accent">{k.last_error}</Text>}
                        </VStack>
                      </StackItem>
                      {k.kind === 'sandbox' && <Token color="purple" label="Sandbox" />}
                    </HStack>
                  </Card>
                ))}
                {(c.connectors ?? []).some(k => k.kind === 'sandbox') && !(c.connectors ?? []).some(k => ['ses', 'resend'].includes(k.kind) && k.health === 'ok') && (
                  <Banner status="info" title="This workspace sends to a sandbox" description="Approved replies are recorded as sent but reach nobody until your domain is verified and a From address is set above." />
                )}
              </Section>

              <Section title="SMS" description="SignalWire numbers connected to this workspace. Replies use the number that received the message.">
                <SMSSetup />
              </Section>

              <Section title="Fax" description="Send a PDF or receive faxes through an assigned SignalWire number.">
                <FaxSetup />
              </Section>

              <Section title="Mail" description="Letters printed and posted by your own Lob account, with USPS tracking on the contact's thread.">
                <MailSetup />
              </Section>

              <Section title="Slack" description="Each website chat is copied into a Slack channel as its own thread, and a reply typed in that thread goes to the visitor.">
                <SlackSetup />
              </Section>

              <Section
                title="Business systems"
                description="Your booking, quoting or CRM system tells us where each customer's inquiry stands. Any system can do it with one API call, or a Zapier or Make step.">
                {(c.systems ?? []).map(s => (
                  <Card key={s.system}>
                    <HStack gap={3} vAlign="center" wrap="wrap">
                      <StatusDot variant="success" label="Connected" />
                      <StackItem size="fill">
                        <VStack gap={0.5}>
                          <Text weight="semibold">{s.system}</Text>
                          <Text type="supporting">
                            {s.records} linked records
                            {s.last_pushed && (
                              <>
                                {' '}
                                · last update <Timestamp value={s.last_pushed} format="relative" />
                              </>
                            )}
                          </Text>
                        </VStack>
                      </StackItem>
                    </HStack>
                  </Card>
                ))}
                <CodeBlock code={LINKS_EXAMPLE} language="http" title="Push a record's state (API coming next)" />
                <Text type="supporting">
                  When a pushed state is missing or older than its limit, any send that depends on it is held for a person.
                </Text>
              </Section>

              <Section title="Agents" description="Each agent has its own token, policy and line in the audit log.">
                {(c.agents ?? []).map(a => (
                  <Card key={a.id}>
                    <HStack gap={3} vAlign="center" wrap="wrap">
                      <StatusDot variant={a.revoked_at ? 'neutral' : 'success'} label={a.revoked_at ? 'Revoked' : 'Active'} />
                      <StackItem size="fill">
                        <VStack gap={0.5}>
                          <Text weight="semibold">{a.name}</Text>
                          <Text type="supporting">{a.may_send ? 'May send new messages without approval. Opt-outs and take-overs still apply.' : AUTONOMY[a.autonomy]}</Text>
                          {a.config_version && <Text type="supporting">Configuration {a.config_version}</Text>}
                        </VStack>
                      </StackItem>
                      {a.can_send && <AgentSending id={a.id} name={a.name} allowed={a.may_send} />}
                      {!a.revoked_at && <RevokeAgent id={a.id} name={a.name} />}
                    </HStack>
                  </Card>
                ))}
              </Section>
            </VStack>
          )}
        </LayoutContent>
      }
    />
  );
}

function Section({title, description, children}: {title: string; description: string; children: React.ReactNode}) {
  return (
    <VStack gap={3}>
      <VStack gap={0.5}>
        <Heading level={2}>{title}</Heading>
        <Text type="supporting">{description}</Text>
      </VStack>
      <VStack gap={2}>{children}</VStack>
    </VStack>
  );
}

// Allowing a token to send means its messages reach customers with no person
// looking first, so turning it on asks twice. Turning it off does not.
function AgentSending({id, name, allowed}: {id: string; name: string; allowed: boolean}) {
  const client = useQueryClient();
  const [sure, setSure] = useState(false);
  const change = useMutation({
    mutationFn: (next: boolean) => api.post(`/agents/${id}/sending`, {allowed: next}),
    onSuccess: () => {
      setSure(false);
      client.invalidateQueries({queryKey: ['connections']});
    },
  });
  if (allowed) {
    return <Button label="Stop sending" size="sm" variant="ghost" isLoading={change.isPending} onClick={() => change.mutate(false)} />;
  }
  return sure ? (
    <HStack gap={1}>
      <Button label={`Let ${name} send without approval`} size="sm" variant="primary" isLoading={change.isPending} onClick={() => change.mutate(true)} />
      <Button label="Keep approvals" size="sm" variant="ghost" onClick={() => setSure(false)} />
    </HStack>
  ) : (
    <Button label="Allow sending" size="sm" variant="ghost" onClick={() => setSure(true)} />
  );
}

// Revoking stops the agent at once; its drafts stay for a person to decide.
function RevokeAgent({id, name}: {id: string; name: string}) {
  const client = useQueryClient();
  const [sure, setSure] = useState(false);
  const revoke = useMutation({
    mutationFn: () => api.post(`/agents/${id}/revoke`, {}),
    onSuccess: () => client.invalidateQueries({queryKey: ['connections']}),
  });
  return sure ? (
    <HStack gap={1}>
      <Button label={`Revoke ${name}`} size="sm" variant="primary" isLoading={revoke.isPending} onClick={() => revoke.mutate()} />
      <Button label="Keep" size="sm" variant="ghost" onClick={() => setSure(false)} />
    </HStack>
  ) : (
    <Button label="Revoke" size="sm" variant="ghost" onClick={() => setSure(true)} />
  );
}
