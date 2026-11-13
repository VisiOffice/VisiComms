import {useState} from 'react';
import {useNavigate, useParams} from 'react-router-dom';
import {useQuery, useQueryClient} from '@tanstack/react-query';
import {VStack} from '@astryxdesign/core/Layout';
import {Button} from '@astryxdesign/core/Button';
import {Banner} from '@astryxdesign/core/Banner';
import {Spinner} from '@astryxdesign/core/Spinner';
import {Text} from '@astryxdesign/core/Text';
import {Link} from '@astryxdesign/core/Link';
import {Icon} from '@astryxdesign/core/Icon';
import {FingerPrintIcon} from '@heroicons/react/24/outline';

import {api, ApiError, type Me} from '../api';
import {AuthFrame} from '../components/AuthFrame';
import {passkeyErrorMessage, registerPasskey} from '../passkeys';

type Invite = {workspace_name: string; name: string; email: string};

export function InvitePage() {
  const {token = ''} = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const invite = useQuery({queryKey: ['invite', token], queryFn: () => api.get<Invite>(`/invites/${token}`)});
  // Signed in already: join under the same login and switch to it.
  const me = useQuery({queryKey: ['me'], queryFn: () => api.get<Me>('/me'), retry: false});

  if (invite.isPending) {
    return (
      <AuthFrame title="Join VisiComms" subtitle="Checking your invite…">
        <VStack hAlign="center">
          <Spinner />
        </VStack>
      </AuthFrame>
    );
  }
  if (invite.error) {
    return (
      <AuthFrame title="This invite has expired" subtitle="Invite links work once and last 7 days.">
        <Text justify="center">
          Ask whoever invited you for a new link, or <Link href="/login">sign in</Link> if you already have a passkey.
        </Text>
      </AuthFrame>
    );
  }

  const join = async () => {
    setError(null);
    try {
      await registerPasskey(token);
      await queryClient.invalidateQueries();
      navigate('/', {replace: true});
    } catch (err) {
      setError(passkeyErrorMessage(err));
    }
  };

  const joinAsMe = async () => {
    setError(null);
    try {
      await api.post(`/invites/${token}/accept`);
      queryClient.clear();
      navigate('/', {replace: true});
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Something went wrong. Try again.');
    }
  };

  if (me.data) {
    return (
      <AuthFrame
        title={`Join ${invite.data.workspace_name}`}
        subtitle={`You are signed in as ${me.data.person.name} (${me.data.workspace.name}). Join with this sign-in and switch between workspaces from the menu at the top of the sidebar.`}>
        <VStack gap={3} hAlign="stretch">
          {error && <Banner status="error" title={error} />}
          <Button label={`Join as ${invite.data.name}`} variant="primary" size="lg" clickAction={joinAsMe} />
        </VStack>
      </AuthFrame>
    );
  }

  return (
    <AuthFrame
      title={`Join ${invite.data.workspace_name}`}
      subtitle={`Invited as ${invite.data.name} (${invite.data.email}). There is no password: your device keeps a passkey instead.`}>
      <VStack gap={3} hAlign="stretch">
        {error && <Banner status="error" title={error} />}
        <Button label="Create a passkey and join" variant="primary" size="lg" icon={<Icon icon={FingerPrintIcon} />} clickAction={join} />
      </VStack>
    </AuthFrame>
  );
}
