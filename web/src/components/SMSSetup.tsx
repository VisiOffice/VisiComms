import {useQuery} from '@tanstack/react-query';
import {VStack} from '@astryxdesign/core/Layout';
import {Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Spinner} from '@astryxdesign/core/Spinner';
import {api, type SMSSettings} from '../api';
import {CopyLine} from './SetupChecklist';

// Kept inside the existing Connections page's sections and spacing.
export function SMSSetup() {
  const query = useQuery({queryKey: ['sms'], queryFn: () => api.get<SMSSettings>('/sms')});
  if (query.isPending) return <Spinner />;
  if (query.error || !query.data) return <Banner status="error" title="SMS settings could not be loaded." />;
  const settings = query.data;
  if (!settings.available) return <Banner status="info" title="SignalWire is not configured on this server" description="The server operator must configure the SignalWire connection before SMS can be received or sent." />;
  return (
    <VStack gap={3}>
      {settings.numbers.length === 0 ? (
        <Text>No SMS number is assigned yet. Ask the server operator to connect an existing number to this workspace.</Text>
      ) : settings.numbers.map(n => <Text key={n.number} type="code">{n.number}</Text>)}
      <CopyLine label="Configure each number’s Compatibility SMS webhook (POST)" value={settings.inbound_url} />
      <Text type="supporting">Delivery callbacks are set automatically on outbound messages. STOP blocks SMS across this contact’s conversations. START clears a keyword STOP, but does not remove a manual block.</Text>
    </VStack>
  );
}
