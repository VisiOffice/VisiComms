import {useEffect, useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {Spinner} from '@astryxdesign/core/Spinner';
import {TextArea} from '@astryxdesign/core/TextArea';
import {TextInput} from '@astryxdesign/core/TextInput';

import {api, type CallMode, type Entrance, type EntrancesResponse, type SIPAccount} from '../api';
import {CopyLine} from './SetupChecklist';
import {ActionPicker, AgentEditor, describe, usesAI} from '../pages/Phone';

// Entrances are door intercoms. A visitor pressing the button calls the
// workspace, and the entrance's own setting answers instead of the number's.
export function Entrances() {
  const client = useQueryClient();
  const q = useQuery({queryKey: ['entrances'], queryFn: () => api.get<EntrancesResponse>('/entrances')});
  const add = useMutation({
    mutationFn: () => api.post<Entrance>('/entrances', {name: 'Front gate'}),
    onSuccess: () => client.invalidateQueries({queryKey: ['entrances']}),
  });
  return (
    <VStack gap={4}>
      <VStack gap={0.5}>
        <Heading level={2}>Entrances</Heading>
        <Text type="supporting">
          Door and gate intercoms. When a visitor presses the button, the AI answers as the gatekeeper: it asks who they are and who they’re here for, never opens the door, and
          leaves you a message.
        </Text>
      </VStack>
      {q.isPending ? (
        <Spinner />
      ) : q.error || !q.data ? (
        <Banner status="error" title="Entrances could not be loaded." />
      ) : (
        <>
          {q.data.entrances.map(e => (
            <EntranceCard key={e.id} entrance={e} settings={q.data} />
          ))}
          {add.error && <Banner status="error" title={add.error.message} />}
          <HStack>
            <Button label="Add an entrance" isLoading={add.isPending} onClick={() => add.mutate()} />
          </HStack>
        </>
      )}
    </VStack>
  );
}

function EntranceCard({entrance, settings}: {entrance: Entrance; settings: EntrancesResponse}) {
  const client = useQueryClient();
  const [draft, setDraft] = useState<Entrance>(entrance);
  useEffect(() => setDraft(entrance), [entrance]);
  const [confirming, setConfirming] = useState(false);
  // The password, shown once, right after the account is made.
  const [created, setCreated] = useState<SIPAccount | null>(null);
  const refresh = () => client.invalidateQueries({queryKey: ['entrances']});
  const path = `/entrances/${entrance.id}`;
  const save = useMutation({mutationFn: () => api.put(path, {name: draft.name, caller_id: draft.caller_id ?? '', flow: draft.flow}), onSuccess: refresh});
  const remove = useMutation({mutationFn: () => api.delete(path), onSuccess: refresh});
  const makeSIP = useMutation({
    mutationFn: () => api.post<SIPAccount>(`${path}/sip`),
    onSuccess: account => {
      setCreated(account);
      refresh();
    },
  });
  const removeSIP = useMutation({
    mutationFn: () => api.delete(`${path}/sip`),
    onSuccess: () => {
      setCreated(null);
      refresh();
    },
  });
  const changed = JSON.stringify(draft) !== JSON.stringify(entrance);
  const can = (mode: CallMode) => mode !== 'ivr' && !!settings.available_modes[mode];
  const flow = draft.flow;
  const sip = created ?? entrance.sip;

  return (
    <Card>
      <VStack gap={4}>
        <VStack gap={0.5}>
          <Heading level={3}>{entrance.name}</Heading>
          <Text type="supporting">Now: {describe(entrance.flow)}</Text>
        </VStack>

        <HStack gap={2} wrap="wrap" vAlign="end">
          <TextInput label="Name" description="Shown on its calls, e.g. “Front gate”." value={draft.name} onChange={v => setDraft(d => ({...d, name: v}))} />
          <TextInput
            label="Caller ID"
            description="Optional. If the intercom reaches you through a bridge that calls your number from a fixed number, enter that number."
            value={draft.caller_id ?? ''}
            placeholder="+13105550100"
            onChange={v => setDraft(d => ({...d, caller_id: v}))}
          />
        </HStack>

        <ActionPicker label="When someone rings" action={flow.answer} can={can} onChange={answer => setDraft(d => ({...d, flow: {...d.flow, answer}}))} />

        {usesAI(flow) && (
          <AgentEditor
            agent={flow.agent ?? settings.default_flow.agent ?? {opening: '', instructions: '', voice: 'marin'}}
            voices={settings.voices}
            rulesLabel="Visitor rules"
            rulesDescription="What to tell delivery drivers, guests and vendors. The AI never opens the door and never says whether anyone is home."
            onChange={agent => setDraft(d => ({...d, flow: {...d.flow, agent}}))}
          />
        )}

        <TextArea label="Message greeting" description="Read to the visitor before the beep, if nobody answers." value={flow.greeting} rows={2} maxLength={500} onChange={v => setDraft(d => ({...d, flow: {...d.flow, greeting: v}}))} />

        {save.error && <Banner status="error" title={save.error.message} />}
        <HStack gap={2} wrap="wrap">
          <Button label="Save" variant="primary" isDisabled={!changed} isLoading={save.isPending} onClick={() => save.mutate()} />
          {changed && <Button label="Undo changes" onClick={() => setDraft(entrance)} />}
          <StackItem size="fill" />
          {confirming ? (
            <>
              <Button label="Remove for good" variant="destructive" isLoading={remove.isPending} onClick={() => remove.mutate()} />
              <Button label="Keep it" onClick={() => setConfirming(false)} />
            </>
          ) : (
            <Button label="Remove" variant="ghost" onClick={() => setConfirming(true)} />
          )}
        </HStack>

        <VStack gap={3}>
          <Heading level={4}>Connect the intercom directly</Heading>
          {!settings.sip_available ? (
            <Text type="supporting">Connect SignalWire for this workspace to give the intercom its own account.</Text>
          ) : !sip ? (
            <>
              <Text type="supporting">
                Give the intercom its own SIP account, and it calls VisiComms straight from the building, with no phone number or on-site bridge in between. Its calls are known
                to be from this entrance by the account they sign in with.
              </Text>
              <HStack>
                <Button label="Make a SIP account" isLoading={makeSIP.isPending} onClick={() => makeSIP.mutate()} />
              </HStack>
            </>
          ) : (
            <>
              <Text type="supporting">
                In UniFi Access, open Interface Designer, choose the intercom, then Doorbell Call → Third-Party SIP → Configure. Enter these, then add the recipient number as a
                receiver and ring the doorbell to test.
              </Text>
              <CopyLine label="SIP server" value={sip.server} />
              <CopyLine label="Port and transport" value={`${sip.port} ${sip.transport}`} />
              <CopyLine label="User ID" value={sip.username} />
              {sip.password ? (
                <CopyLine label="Password: copy it now. It won’t be shown again." value={sip.password} />
              ) : (
                <Text type="supporting">Password: shown only when the account was made. If it’s lost, replace the account: the old one stops working and the user ID changes.</Text>
              )}
              <CopyLine label="Recipient number" value={sip.recipient} />
              <HStack gap={2} wrap="wrap">
                <Button label="Replace the SIP account" isLoading={makeSIP.isPending} onClick={() => makeSIP.mutate()} />
                <Button label="Remove the SIP account" variant="ghost" isLoading={removeSIP.isPending} onClick={() => removeSIP.mutate()} />
              </HStack>
            </>
          )}
          {(makeSIP.error || removeSIP.error) && <Banner status="error" title={(makeSIP.error ?? removeSIP.error)!.message} />}
        </VStack>
      </VStack>
    </Card>
  );
}
