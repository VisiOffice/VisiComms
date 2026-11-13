import {useNavigate, useParams} from 'react-router-dom';
import {Layout, LayoutContent} from '@astryxdesign/core/Layout';

import {ThreadPane} from '../components/ThreadPane';

// A thread on its own screen: where the queue sends you on a phone, and
// where a shared link to a thread lands.
export function ThreadPage() {
  const {id = ''} = useParams();
  const navigate = useNavigate();
  return (
    <Layout
      height="fill"
      contentWidth={820}
      content={
        <LayoutContent padding={4} isScrollable>
          <ThreadPane threadId={id} onClose={() => navigate('/')} isFullWidth />
        </LayoutContent>
      }
    />
  );
}
