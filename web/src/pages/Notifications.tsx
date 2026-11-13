import {useEffect, useState} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {Layout, LayoutContent, LayoutHeader, HStack, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {CheckboxInput} from '@astryxdesign/core/CheckboxInput';
import {Selector} from '@astryxdesign/core/Selector';
import {Spinner} from '@astryxdesign/core/Spinner';
import {Table, proportional} from '@astryxdesign/core/Table';
import {TextInput} from '@astryxdesign/core/TextInput';

import {api, type Delivery, type NotificationSetting, type NotificationsResponse, type NotifiedPerson, type NotifyChannel} from '../api';

const DELIVERY: {value: Delivery; label: string}[] = [
  {value: 'instant', label: 'Right away'},
  {value: 'daily', label: 'Daily summary'},
  {value: 'off', label: 'Off'},
];

export function NotificationsPage() {
  const q = useQuery({queryKey: ['notifications'], queryFn: () => api.get<NotificationsResponse>('/notifications')});
  const me = q.data?.people[0];
  return (
    <Layout
      height="fill"
      contentWidth={880}
      header={
        <LayoutHeader>
          <VStack gap={0.5}>
            <Heading level={1}>Notifications</Heading>
            <Text type="supporting">What you are told about, and how. The daily summary is emailed at 8am. An agent can change these for you when you ask it to.</Text>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent padding={4}>
          {q.isPending ? <Spinner /> : q.error || !q.data || !me ? <Banner status="error" title="Notification settings could not be loaded." /> : <Settings person={me} data={q.data} />}
        </LayoutContent>
      }
    />
  );
}

function Settings({person, data}: {person: NotifiedPerson; data: NotificationsResponse}) {
  const client = useQueryClient();
  const [settings, setSettings] = useState<NotificationSetting[]>(person.settings);
  const [phone, setPhone] = useState(person.phone ?? '');
  useEffect(() => {
    setSettings(person.settings);
    setPhone(person.phone ?? '');
  }, [person]);
  const save = useMutation({
    mutationFn: () => api.put('/notifications', {settings, phone}),
    onSuccess: () => client.invalidateQueries({queryKey: ['notifications']}),
  });
  const changed = JSON.stringify(settings) !== JSON.stringify(person.settings) || phone !== (person.phone ?? '');
  const update = (category: string, change: Partial<NotificationSetting>) => setSettings(all => all.map(s => (s.category === category ? {...s, ...change} : s)));
  const toggle = (s: NotificationSetting, channel: NotifyChannel, on: boolean) => {
    const channels = on ? [...s.channels, channel] : s.channels.filter(c => c !== channel);
    update(s.category, {channels: channels.length ? channels.sort() : ['email']});
  };
  const wantsText = settings.some(s => s.delivery === 'instant' && s.channels.includes('sms'));
  const rows = data.categories.map(c => ({...c, setting: settings.find(s => s.category === c.key)!}));

  return (
    <VStack gap={4}>
      {!data.channels.email && <Banner status="warning" title="Email notifications are not set up on this server" description="The operator needs to set NOTIFY_FROM to a verified sending address. Until then nothing is emailed." />}
      <Table
        data={rows}
        idKey="key"
        density="balanced"
        columns={[
          {key: 'label', header: 'When', width: proportional(2, {minWidth: 200}), renderCell: r => (
            <VStack gap={0.5}>
              <Text weight="semibold">{r.label}</Text>
              <Text type="supporting">{r.description}</Text>
            </VStack>
          )},
          {key: 'delivery', header: 'Tell me', width: proportional(1, {minWidth: 160}), renderCell: r => (
            <Selector label={`${r.label}: tell me`} isLabelHidden value={r.setting.delivery} options={DELIVERY} onChange={v => update(r.key, {delivery: v as Delivery})} />
          )},
          {key: 'channels', header: 'By', width: proportional(1, {minWidth: 160}), renderCell: r =>
            r.setting.delivery === 'instant' ? (
              <HStack gap={3}>
                <CheckboxInput label="Email" value={r.setting.channels.includes('email')} onChange={on => toggle(r.setting, 'email', on)} />
                <CheckboxInput label="Text" value={r.setting.channels.includes('sms')} isDisabled={!data.channels.sms} onChange={on => toggle(r.setting, 'sms', on)} />
              </HStack>
            ) : (
              <Text type="supporting">{r.setting.delivery === 'daily' ? 'Email' : '—'}</Text>
            ),
          },
        ]}
      />
      <TextInput
        label="Mobile number for texts"
        description="Texts come from one of this workspace’s numbers."
        value={phone}
        placeholder="+13105550100"
        onChange={setPhone}
        status={wantsText && !phone ? {type: 'warning', message: 'Add a mobile number, or texts cannot be sent.'} : undefined}
      />
      {save.error && <Banner status="error" title={save.error.message} />}
      <HStack gap={2}>
        <Button label="Save" variant="primary" isDisabled={!changed} isLoading={save.isPending} onClick={() => save.mutate()} />
        {changed && <Button label="Undo changes" onClick={() => { setSettings(person.settings); setPhone(person.phone ?? ''); }} />}
      </HStack>
    </VStack>
  );
}
