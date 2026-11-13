import {useState} from 'react';
import {useParams} from 'react-router-dom';
import {useMutation, useQuery} from '@tanstack/react-query';
import {Layout, LayoutContent, LayoutHeader, HStack, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {CheckboxInput} from '@astryxdesign/core/CheckboxInput';
import {Spinner} from '@astryxdesign/core/Spinner';

import {api, type OAuthRequest} from '../api';

// The consent page for an MCP client such as Claude: the person picks which
// workspaces it may act in. Each gets its own agent, in approve mode.
export function AuthorizePage() {
  const {request} = useParams();
  const q = useQuery({queryKey: ['oauth-request', request], queryFn: () => api.get<OAuthRequest>(`/oauth/requests/${request}`), retry: false});
  const [picked, setPicked] = useState<string[] | null>(null);
  const chosen = picked ?? q.data?.workspaces.map(w => w.id) ?? [];
  const go = (r: {redirect: string}) => {
    window.location.assign(r.redirect);
  };
  const allow = useMutation({mutationFn: () => api.post<{redirect: string}>(`/oauth/requests/${request}/approve`, {workspaces: chosen}), onSuccess: go});
  const deny = useMutation({mutationFn: () => api.post<{redirect: string}>(`/oauth/requests/${request}/deny`, {}), onSuccess: go});
  const done = allow.isSuccess || deny.isSuccess;

  return (
    <Layout
      height="fill"
      contentWidth={560}
      header={
        <LayoutHeader>
          <Heading level={1}>Connect an app</Heading>
        </LayoutHeader>
      }
      content={
        <LayoutContent padding={4}>
          {q.isPending ? (
            <Spinner />
          ) : q.error || !q.data ? (
            <Banner status="error" title={(q.error as Error)?.message || 'This request could not be loaded.'} />
          ) : done ? (
            <Text>Returning you to {q.data.client_name}…</Text>
          ) : (
            <Card padding={4}>
              <VStack gap={4}>
                <VStack gap={1}>
                  <Heading level={2}>{q.data.client_name} wants to work in your VisiComms inbox</Heading>
                  <Text type="supporting">The name is what the app called itself. After you choose, you go back to {q.data.redirect_host}.</Text>
                </VStack>
                <VStack gap={1}>
                  <Text>It will be able to:</Text>
                  <Text>• read your threads, contacts and the replies waiting for approval</Text>
                  <Text>• draft replies, which wait for a person to approve, edit or reject</Text>
                  <Text>• mark threads done or hand them to a person</Text>
                  <Text>• change how calls are answered and who is notified, when you ask it to</Text>
                </VStack>
                <VStack gap={2}>
                  <Text weight="semibold">Workspaces it may use</Text>
                  {q.data.workspaces.map(w => (
                    <CheckboxInput
                      key={w.id}
                      label={w.name}
                      value={chosen.includes(w.id)}
                      onChange={on => setPicked(on ? [...chosen, w.id] : chosen.filter(id => id !== w.id))}
                    />
                  ))}
                  <Text type="supporting">It acts as its own agent in each, starting in approve mode. You can revoke it under Connections, Agents.</Text>
                </VStack>
                {(allow.error || deny.error) && <Banner status="error" title={((allow.error || deny.error) as Error).message} />}
                <HStack gap={2}>
                  <Button label="Allow" variant="primary" isDisabled={chosen.length === 0} isLoading={allow.isPending} onClick={() => allow.mutate()} />
                  <Button label="Deny" variant="ghost" isLoading={deny.isPending} onClick={() => deny.mutate()} />
                </HStack>
              </VStack>
            </Card>
          )}
        </LayoutContent>
      }
    />
  );
}
