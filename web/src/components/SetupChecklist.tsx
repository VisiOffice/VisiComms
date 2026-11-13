import {useState} from 'react';
import {useMutation, useQueryClient} from '@tanstack/react-query';
import {HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {Icon} from '@astryxdesign/core/Icon';
import {IconButton} from '@astryxdesign/core/IconButton';
import {ProgressBar} from '@astryxdesign/core/ProgressBar';
import {Stepper, Step} from '@astryxdesign/core/Stepper';
import {TextInput} from '@astryxdesign/core/TextInput';
import {useToast} from '@astryxdesign/core/Toast';
import {useMediaQuery} from '@astryxdesign/core/hooks';
import {CheckIcon, ClipboardDocumentIcon, ExclamationTriangleIcon} from '@heroicons/react/24/outline';

import {api, ApiError, type SetupStatus, type SetupStep} from '../api';

// What a new workspace still needs, read from what is actually configured.
// Steps can be done in any order, so each step's mark comes from its own
// state rather than from its position.
export function SetupChecklist({status, workspace}: {status: SetupStatus; workspace: string}) {
  const isNarrow = useMediaQuery('(max-width: 600px)');
  const required = status.steps.filter(s => !s.optional);
  const done = required.filter(s => s.state === 'done').length;
  const firstOpen = Math.max(0, status.steps.findIndex(s => s.state !== 'done'));

  return (
    <Card padding={isNarrow ? 4 : 6} maxWidth={720} width="100%">
      <VStack gap={5}>
        <VStack gap={2}>
          <Heading level={2}>Set up {workspace}</Heading>
          <Text color="secondary">
            Agents can't do anything until mail comes in, replies can go out, and there are approved facts to quote. {done} of {required.length}{' '}
            done.
          </Text>
          <ProgressBar value={done} max={required.length} label="Setup progress" isLabelHidden />
        </VStack>
        <Stepper activeStep={firstOpen} orientation="vertical">
          {status.steps.map((s, i) => (
            <Step
              key={s.key}
              step={i}
              label={s.title}
              description={s.detail}
              isOptional={s.optional}
              status={s.state === 'done' ? 'success' : s.state === 'partial' ? 'warning' : undefined}
              indicator={
                s.state === 'done' ? (
                  <Icon icon={CheckIcon} size="sm" />
                ) : s.state === 'partial' ? (
                  <Icon icon={ExclamationTriangleIcon} size="sm" />
                ) : (
                  'number'
                )
              }>
              <StepAction step={s} />
            </Step>
          ))}
        </Stepper>
      </VStack>
    </Card>
  );
}

function StepAction({step}: {step: SetupStep}) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const refresh = () => queryClient.invalidateQueries({queryKey: ['setup']});
  const fail = (err: unknown) => toast({body: err instanceof ApiError ? err.message : 'That didn’t work. Try again.', type: 'error'});

  const inbox = useMutation({mutationFn: () => api.post<{address: string}>('/setup/inbox'), onSuccess: refresh, onError: fail});
  const sandbox = useMutation({mutationFn: () => api.post('/setup/sandbox'), onSuccess: refresh, onError: fail});

  switch (step.key) {
    case 'inbox':
      return step.state === 'done' && step.value ? (
        <CopyLine label="In your email provider, forward your inquiries address to:" value={step.value} />
      ) : (
        <Button label="Create a forwarding address" variant="primary" isLoading={inbox.isPending} onClick={() => inbox.mutate()} />
      );
    case 'sending':
      return step.state === 'done' ? null : (
        <HStack gap={2} wrap="wrap">
          <Button label="Set up your domain" variant="primary" href="/connections" />
          {step.state === 'todo' && <Button label="Use the sandbox for now" isLoading={sandbox.isPending} onClick={() => sandbox.mutate()} />}
        </HStack>
      );
    case 'approved_info':
      return step.state === 'done' ? null : (
        <VStack gap={2} hAlign="start">
          <Button label="Open rules and approved info" href="/rules" />
          {step.state === 'todo' && <Text type="supporting">There is no editor yet; it is coming with the agent runner.</Text>}
        </VStack>
      );
    case 'agent':
      return step.state === 'done' ? null : <Text type="supporting">The agent runner is being built. It will appear here when it can draft replies.</Text>;
    case 'team':
      return <InviteForm />;
    default:
      return null;
  }
}

function InviteForm() {
  const toast = useToast();
  const [name, setName] = useState('');
  const [email, setEmail] = useState('');
  const [link, setLink] = useState<string | null>(null);
  const invite = useMutation({
    mutationFn: () => api.post<{url: string}>('/invites', {name, email}),
    onSuccess: res => {
      setLink(res.url);
      setName('');
      setEmail('');
    },
    onError: err => toast({body: err instanceof ApiError ? err.message : 'Could not create the invite.', type: 'error'}),
  });

  return (
    <VStack gap={3} hAlign="stretch">
      <HStack gap={2} wrap="wrap" vAlign="end">
        <TextInput label="Name" value={name} onChange={setName} width={200} />
        <TextInput label="Email" type="email" value={email} onChange={setEmail} width={240} onEnter={() => invite.mutate()} />
        <Button label="Create invite link" isLoading={invite.isPending} isDisabled={!name || !email} onClick={() => invite.mutate()} />
      </HStack>
      {link && <CopyLine label="Invite link: send it to them. It works once and lasts 7 days." value={link} />}
    </VStack>
  );
}

export function CopyLine({label, value}: {label: string; value: string}) {
  const toast = useToast();
  return (
    <VStack gap={1}>
      <Text type="supporting">{label}</Text>
      <HStack gap={2} vAlign="center">
        <StackItem size="fill">
          {/* Wraps rather than truncating: on a phone the whole address has to
              be readable, and a long value must not widen the page. */}
          <Text type="code" wordBreak="break-all">
            {value}
          </Text>
        </StackItem>
        <IconButton
          variant="ghost"
          size="sm"
          label="Copy"
          tooltip="Copy"
          icon={<Icon icon={ClipboardDocumentIcon} size="sm" />}
          onClick={async () => {
            await navigator.clipboard.writeText(value);
            toast({body: 'Copied.'});
          }}
        />
      </HStack>
    </VStack>
  );
}
