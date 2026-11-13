import {useState} from 'react';
import {useLocation, useNavigate} from 'react-router-dom';
import {useQueryClient} from '@tanstack/react-query';
import {VStack} from '@astryxdesign/core/Layout';
import {Button} from '@astryxdesign/core/Button';
import {Banner} from '@astryxdesign/core/Banner';
import {Text} from '@astryxdesign/core/Text';
import {Icon} from '@astryxdesign/core/Icon';
import {FingerPrintIcon} from '@heroicons/react/24/outline';

import {AuthFrame} from '../components/AuthFrame';
import {passkeyErrorMessage, signInWithPasskey} from '../passkeys';

export function LoginPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);

  const signIn = async () => {
    setError(null);
    try {
      await signInWithPasskey();
      await queryClient.invalidateQueries();
      const from = (location.state as {from?: string} | null)?.from;
      navigate(from && from !== '/login' ? from : '/', {replace: true});
    } catch (err) {
      setError(passkeyErrorMessage(err));
    }
  };

  return (
    <AuthFrame
      title="Sign in"
      subtitle="Use the passkey you saved for VisiComms."
      footer={
        <Text type="supporting" color="secondary" justify="center">
          New here? Ask your workspace owner for an invite link.
        </Text>
      }>
      <VStack gap={3} hAlign="stretch">
        {error && <Banner status="error" title={error} />}
        <Button label="Sign in with a passkey" variant="primary" size="lg" icon={<Icon icon={FingerPrintIcon} />} clickAction={signIn} />
      </VStack>
    </AuthFrame>
  );
}
