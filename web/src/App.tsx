import {Navigate, Route, Routes, useLocation} from 'react-router-dom';
import {useQuery} from '@tanstack/react-query';
import {Center} from '@astryxdesign/core/Center';
import {Spinner} from '@astryxdesign/core/Spinner';

import {api, ApiError, type Me} from './api';
import {Shell} from './components/Shell';
import {LoginPage} from './pages/Login';
import {AuthorizePage} from './pages/Authorize';
import {InvitePage} from './pages/Invite';
import {AttentionPage} from './pages/Attention';
import {ThreadPage} from './pages/ThreadPage';
import {PhonePage} from './pages/Phone';
import {NotificationsPage} from './pages/Notifications';
import {RulesPage} from './pages/Rules';
import {ConnectionsPage} from './pages/Connections';
import {WebChatPage} from './pages/WebChat';

export function useMe() {
  return useQuery({queryKey: ['me'], queryFn: () => api.get<Me>('/me')});
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/invite/:token" element={<InvitePage />} />
      <Route path="/*" element={<SignedIn />} />
    </Routes>
  );
}

function SignedIn() {
  const me = useMe();
  const location = useLocation();
  if (me.isPending) {
    return (
      <Center axis="both" height="100dvh">
        <Spinner size="lg" />
      </Center>
    );
  }
  if (me.error instanceof ApiError && me.error.status === 401) {
    return <Navigate to="/login" replace state={{from: location.pathname}} />;
  }
  if (me.error || !me.data) {
    throw me.error ?? new Error('Could not load your account');
  }
  return (
    <Shell me={me.data}>
      <Routes>
        <Route path="/" element={<AttentionPage />} />
        <Route path="/threads/:id" element={<ThreadPage />} />
        <Route path="/rules" element={<RulesPage />} />
        <Route path="/connections" element={<ConnectionsPage />} />
        <Route path="/phone" element={<PhonePage />} />
        <Route path="/chat" element={<WebChatPage />} />
        <Route path="/authorize/:request" element={<AuthorizePage />} />
        <Route path="/settings/notifications" element={<NotificationsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Shell>
  );
}
