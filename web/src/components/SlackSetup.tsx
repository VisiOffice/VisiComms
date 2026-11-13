import {useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {Selector} from '@astryxdesign/core/Selector';
import {Spinner} from '@astryxdesign/core/Spinner';
import {StatusDot} from '@astryxdesign/core/StatusDot';
import {TextInput} from '@astryxdesign/core/TextInput';
import {useToast} from '@astryxdesign/core/Toast';

import {api, type SlackMember, type SlackSettings} from '../api';
import {CopyLine} from './SetupChecklist';

// Slack: website chats copied into a channel, one Slack thread per chat, and
// replies typed in that thread sent to the visitor.
export function SlackSetup() {
  const q = useQuery({queryKey: ['slack'], queryFn: () => api.get<SlackSettings>('/slack')});
  const [editing, setEditing] = useState(false);
  if (q.isPending) return <Spinner />;
  if (q.error || !q.data) return <Banner status="error" title="Slack settings could not be loaded." />;
  const s = q.data;
  if (!s.available) return <Text>The server operator must set VISICOMMS_MASTER_KEY before Slack can be connected.</Text>;
  if (!s.connected || editing) return <SlackForm current={s} onDone={() => setEditing(false)} />;
  return (
    <VStack gap={3}>
      <Card>
        <HStack gap={3} vAlign="center" wrap="wrap">
          <StatusDot variant={s.health === 'failing' ? 'error' : 'success'} label={s.health === 'failing' ? 'Failing' : 'Connected'} />
          <StackItem size="fill">
            <VStack gap={0.5}>
              <Text weight="semibold">
                {s.settings.team_name ? `${s.settings.team_name}, ` : ''}channel {s.settings.channel_id}
              </Text>
              <Text type="supporting">
                {s.settings.mention && s.settings.mention_on !== 'never'
                  ? `Mentions ${s.settings.mention.replace(/^<!?|>$/g, '@')} ${s.settings.mention_on === 'every' ? 'on every visitor message' : 'on each new chat'}`
                  : 'No mention'}
              </Text>
              {s.health === 'failing' && s.last_error && <Text type="supporting">{s.last_error}</Text>}
            </VStack>
          </StackItem>
          <Button label="Change" variant="ghost" size="sm" onClick={() => setEditing(true)} />
        </HStack>
      </Card>
      <CopyLine label="In the Slack app, Event Subscriptions, Request URL (subscribe the bot to message.channels, or message.groups for a private channel)" value={s.events_url} />
      <SlackMembers members={s.members} />
      <Disconnect />
    </VStack>
  );
}

function SlackForm({current, onDone}: {current: SlackSettings; onDone: () => void}) {
  const client = useQueryClient();
  const toast = useToast();
  const [token, setToken] = useState('');
  const [secret, setSecret] = useState('');
  const [channel, setChannel] = useState(current.settings?.channel_id ?? '');
  const [mention, setMention] = useState((current.settings?.mention ?? '').replace(/^<!(here|channel)>$/, '@$1'));
  const [mentionOn, setMentionOn] = useState(current.settings?.mention_on || 'new');
  const save = useMutation({
    mutationFn: () => api.put<SlackSettings>('/slack', {bot_token: token, signing_secret: secret, channel_id: channel, mention, mention_on: mentionOn}),
    onSuccess: async () => {
      toast({body: 'Slack connected. A test message was posted to the channel.'});
      await client.invalidateQueries({queryKey: ['slack']});
      onDone();
    },
  });
  const keep = current.connected ? 'Leave empty to keep the saved one.' : undefined;
  return (
    <Card padding={4}>
      <VStack gap={3}>
        <Heading level={3}>{current.connected ? 'Change Slack' : 'Connect Slack'}</Heading>
        <Text type="supporting">
          Create a Slack app for your workspace with the bot permissions chat:write, users:read, users:read.email and channels:history (groups:history for a private
          channel), install it, and invite it to the channel with /invite.
        </Text>
        <TextInput label="Bot User OAuth Token" description={keep ?? 'Starts with xoxb-, on the app’s OAuth & Permissions page.'} type="password" value={token} onChange={setToken} />
        <TextInput label="Signing secret" description={keep ?? 'On the app’s Basic Information page.'} type="password" value={secret} onChange={setSecret} />
        <TextInput label="Channel ID" description="In the channel’s details, at the bottom, such as C0123ABCDEF." value={channel} onChange={setChannel} />
        <HStack gap={3} wrap="wrap">
          <TextInput label="Mention (optional)" description="@here, @channel, or a group code such as <!subteam^S0123ABC>." value={mention} onChange={setMention} />
          <Selector
            label="Mention on"
            value={mentionOn}
            options={[
              {value: 'new', label: 'Each new chat'},
              {value: 'every', label: 'Every visitor message'},
              {value: 'never', label: 'Never'},
            ]}
            onChange={v => setMentionOn(v as SlackSettings['settings']['mention_on'])}
          />
        </HStack>
        {save.error && <Banner status="error" title={(save.error as Error).message} />}
        <HStack gap={2}>
          <Button label={current.connected ? 'Save' : 'Connect'} variant="primary" isLoading={save.isPending} onClick={() => save.mutate()} />
          {current.connected && <Button label="Cancel" variant="ghost" onClick={onDone} />}
        </HStack>
      </VStack>
    </Card>
  );
}

function SlackMembers({members}: {members: SlackMember[]}) {
  return (
    <VStack gap={2}>
      <Heading level={3}>Who can reply from Slack</Heading>
      <Text type="supporting">
        A reply typed in a chat’s Slack thread goes to the visitor as the linked person. People are linked the first time they reply, when their Slack email matches the one
        they sign in with; otherwise paste their Slack member ID (their profile, More, Copy member ID). Replies from anyone not linked are not sent.
      </Text>
      {members.map(m => (
        <MemberRow key={m.id} member={m} />
      ))}
    </VStack>
  );
}

function MemberRow({member}: {member: SlackMember}) {
  const client = useQueryClient();
  const [value, setValue] = useState(member.slack_user_id);
  const save = useMutation({
    mutationFn: () => api.put(`/slack/members/${member.id}`, {slack_user_id: value}),
    onSuccess: () => client.invalidateQueries({queryKey: ['slack']}),
  });
  return (
    <HStack gap={2} vAlign="end" wrap="wrap">
      <StackItem size="fill">
        <TextInput label={member.email ? `${member.name} (${member.email})` : member.name} placeholder="Not linked" value={value} onChange={setValue} />
      </StackItem>
      <Button label="Save" size="sm" variant="ghost" isDisabled={value === member.slack_user_id} isLoading={save.isPending} onClick={() => save.mutate()} />
      {save.error && <Text type="supporting">{(save.error as Error).message}</Text>}
    </HStack>
  );
}

function Disconnect() {
  const client = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.delete('/slack'),
    onSuccess: () => client.invalidateQueries({queryKey: ['slack']}),
  });
  return (
    <HStack>
      <Button label="Disconnect Slack" variant="ghost" isLoading={remove.isPending} onClick={() => remove.mutate()} />
    </HStack>
  );
}
