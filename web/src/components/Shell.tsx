import type {ReactNode} from 'react';
import {useLocation, useNavigate} from 'react-router-dom';
import {useQuery, useQueryClient} from '@tanstack/react-query';
import {AppShell} from '@astryxdesign/core/AppShell';
import {SideNav, SideNavItem, SideNavSection} from '@astryxdesign/core/SideNav';
import {useToast} from '@astryxdesign/core/Toast';
import {Icon} from '@astryxdesign/core/Icon';
import {Badge} from '@astryxdesign/core/Badge';
import {Avatar} from '@astryxdesign/core/Avatar';
import {Button} from '@astryxdesign/core/Button';
import {DropdownMenu} from '@astryxdesign/core/DropdownMenu';
import {HStack, VStack} from '@astryxdesign/core/Layout';
import {Text} from '@astryxdesign/core/Text';
import {
  ArrowRightStartOnRectangleIcon,
  BellIcon,
  CheckIcon,
  PlusIcon,
  ChevronUpDownIcon,
  InboxStackIcon,
  PhoneIcon, PuzzlePieceIcon, ChatBubbleLeftRightIcon,
  ScaleIcon,
} from '@heroicons/react/24/outline';

import {api, ApiError, type AttentionItem, type Me, type Membership} from '../api';
import {linkPasskey, passkeyErrorMessage} from '../passkeys';
import {Softphone} from './Softphone';

export function Shell({me, children}: {me: Me; children: ReactNode}) {
  const {pathname} = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const attention = useQuery({
    queryKey: ['attention'],
    queryFn: () => api.get<{items: AttentionItem[]}>('/attention'),
    refetchInterval: 30_000,
  });
  const count = attention.data?.items.length ?? 0;

  const signOut = async () => {
    await api.post('/auth/logout');
    queryClient.clear();
    navigate('/login');
  };

  const isQueue = pathname === '/' || pathname.startsWith('/threads/');

  return (
    <AppShell
      sideNav={
        <SideNav
          header={
            // The business is the place you are in, as in shadcn's team
            // switcher; the product name stays out of the way.
            <WorkspaceMenu me={me} />
          }
          footer={<PersonMenu me={me} onSignOut={signOut} />}>
          <SideNavSection title="Work" isHeaderHidden>
            <SideNavItem
              label="Needs attention"
              icon={InboxStackIcon}
              href="/"
              isSelected={isQueue}
              endContent={count > 0 ? <Badge label={String(count)} variant="neutral" /> : undefined}
            />
          </SideNavSection>
          <SideNavSection title="Setup">
            <SideNavItem label="Rules and approved info" icon={ScaleIcon} href="/rules" isSelected={pathname === '/rules'} />
            <SideNavItem label="Phone" icon={PhoneIcon} href="/phone" isSelected={pathname === '/phone'} />
            <SideNavItem label="Web chat" icon={ChatBubbleLeftRightIcon} href="/chat" isSelected={pathname === '/chat'} />
            <SideNavItem label="Notifications" icon={BellIcon} href="/settings/notifications" isSelected={pathname === '/settings/notifications'} />
            <SideNavItem label="Connections" icon={PuzzlePieceIcon} href="/connections" isSelected={pathname === '/connections'} />
          </SideNavSection>
          <SideNavSection title="Phone">
            <Softphone />
          </SideNavSection>
        </SideNav>
      }>
      {children}
    </AppShell>
  );
}

// The business you are in, as in shadcn's team switcher: it opens into the
// other workspaces this login belongs to, and adding one by its passkey.
function WorkspaceMenu({me}: {me: Me}) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const toast = useToast();
  const workspaces = useQuery({
    queryKey: ['workspaces'],
    queryFn: () => api.get<{workspaces: Membership[]}>('/workspaces'),
  });
  const others = (workspaces.data?.workspaces ?? []).filter(w => !w.current);

  // Everything cached belongs to the old workspace.
  const reset = async () => {
    queryClient.clear();
    navigate('/', {replace: true});
  };
  const switchTo = async (w: Membership) => {
    try {
      await api.post(`/workspaces/${w.workspace_id}/switch`);
      await reset();
    } catch (err) {
      toast({body: err instanceof ApiError ? err.message : 'Could not switch workspace.', type: 'error'});
    }
  };
  const add = async () => {
    try {
      const list = await linkPasskey();
      queryClient.setQueryData(['workspaces'], {workspaces: list});
      toast({body: 'Added. Pick it from this menu to switch.'});
    } catch (err) {
      toast({body: passkeyErrorMessage(err), type: 'error'});
    }
  };

  return (
    <DropdownMenu
      placement="below"
      items={[
        {label: me.workspace.name, icon: CheckIcon, isDisabled: true},
        ...others.map(w => ({key: w.workspace_id, label: w.name, onClick: () => void switchTo(w)})),
        {type: 'divider' as const},
        {
          label: 'Add a workspace',
          description: "Sign in with that workspace's passkey once",
          icon: PlusIcon,
          onClick: () => void add(),
        },
      ]}
      renderTrigger={trigger => (
        <Button
          {...trigger}
          label={`${me.workspace.name}, switch workspace`}
          variant="ghost"
          size="lg"
          width="100%"
          style={{justifyContent: 'space-between'}}
          endContent={<Icon icon={ChevronUpDownIcon} size="sm" />}>
          <HStack gap={2} vAlign="center">
            <Avatar name={me.workspace.name} shape="rounded" size="sm" tooltip={false} />
            <Text weight="semibold" maxLines={1}>
              {me.workspace.name}
            </Text>
          </HStack>
        </Button>
      )}
    />
  );
}

// Who is signed in, as in shadcn's NavUser: avatar, name and email at the
// foot of the sidebar, opening upward into the account actions.
function PersonMenu({me, onSignOut}: {me: Me; onSignOut: () => void}) {
  return (
    <DropdownMenu
      placement="above"
      items={[{label: 'Sign out', icon: ArrowRightStartOnRectangleIcon, onClick: onSignOut}]}
      renderTrigger={trigger => (
        <Button
          {...trigger}
          label={`${me.person.name}, account menu`}
          variant="ghost"
          size="lg"
          width="100%"
          // Button centres its label; a sidebar row reads from the left with
          // the chevron at the far edge.
          style={{justifyContent: 'space-between'}}
          endContent={<Icon icon={ChevronUpDownIcon} size="sm" />}>
          <HStack gap={2} vAlign="center">
            <Avatar name={me.person.name} shape="rounded" size="sm" tooltip={false} />
            <VStack gap={0} hAlign="start">
              <Text weight="semibold" maxLines={1}>
                {me.person.name}
              </Text>
              <Text type="supporting" maxLines={1}>
                {me.person.email}
              </Text>
            </VStack>
          </HStack>
        </Button>
      )}
    />
  );
}
