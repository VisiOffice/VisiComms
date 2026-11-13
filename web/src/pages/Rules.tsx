import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {Layout, LayoutContent, LayoutHeader, HStack, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {Card} from '@astryxdesign/core/Card';
import {Grid} from '@astryxdesign/core/Grid';
import {Spinner} from '@astryxdesign/core/Spinner';
import {Table, pixel, proportional} from '@astryxdesign/core/Table';
import {Timestamp} from '@astryxdesign/core/Timestamp';
import {Token} from '@astryxdesign/core/Token';
import {useToast} from '@astryxdesign/core/Toast';

import {api, ApiError, type ApprovedInfoContent, type ApprovedInfoVersion} from '../api';

const ORDER = [
  {step: '1 · Platform', title: 'Token and workspace', text: 'Scope, revocation and the workspace kill switch. Applies to reads too.'},
  {step: '2 · Always enforced', title: 'Blocks and required holds', text: 'Opt-out, take-over, unverified recipient, stale linked records.'},
  {step: '3 · Yours to set', title: 'Permissions', text: 'Per action and context, only inside what steps 1 and 2 allow.'},
  {step: '4 · Default', title: 'Everything else is held', text: 'Or denied when no person can approve it.'},
];

type Permission = {id: string; action: string; context: string; treatment: 'Allow' | 'Hold' | 'Always held'; setBy: string};

const PERMISSIONS: Permission[] = [
  {id: 'read', action: 'Read workspace and contact context', context: 'Within the agent token’s scopes', treatment: 'Allow', setBy: 'Default'},
  {id: 'reply', action: 'Reply on an existing thread', context: 'Any', treatment: 'Hold', setBy: 'Default'},
  {id: 'first', action: 'First message to a new contact', context: 'Any', treatment: 'Hold', setBy: 'Default'},
  {id: 'price', action: 'Message with a price, refund or payment link', context: 'Any', treatment: 'Hold', setBy: 'Default'},
  {id: 'dispute', action: 'Handle a dispute or exception', context: 'Any', treatment: 'Always held', setBy: 'Platform'},
  {id: 'rules', action: 'Change a rule or connector', context: 'Any', treatment: 'Always held', setBy: 'Platform'},
];

export function RulesPage() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const info = useQuery({queryKey: ['approved-info'], queryFn: () => api.get<{versions: ApprovedInfoVersion[]}>('/approved-info')});
  const approve = useMutation({
    mutationFn: (id: string) => api.post<{voided_drafts: number}>(`/approved-info/${id}/approve`),
    onSuccess: res => {
      toast({body: res.voided_drafts ? `Approved. ${res.voided_drafts} held drafts that quoted older info were voided.` : 'Approved.'});
      return Promise.all(['approved-info', 'attention', 'thread'].map(k => queryClient.invalidateQueries({queryKey: [k]})));
    },
    onError: err => toast({body: err instanceof ApiError ? err.message : 'Could not approve.', type: 'error'}),
  });

  const versions = info.data?.versions ?? [];
  const live = versions.find(v => v.approved_at);
  const draft = versions.find(v => !v.approved_at && (!live || v.version > live.version));

  return (
    <Layout
      height="fill"
      contentWidth={1040}
      header={
        <LayoutHeader>
          <VStack gap={0.5}>
            <Heading level={1}>Rules and approved info</Heading>
            <Text type="supporting">Every outbound action is checked in this order. A block always beats a permission.</Text>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent padding={4}>
          <VStack gap={6}>
            <Grid columns={{minWidth: 200}} gap={3}>
              {ORDER.map(o => (
                <Card key={o.step}>
                  <VStack gap={1}>
                    <Text type="supporting">{o.step}</Text>
                    <Text weight="semibold">{o.title}</Text>
                    <Text type="supporting">{o.text}</Text>
                  </VStack>
                </Card>
              ))}
            </Grid>

            <VStack gap={3}>
              <VStack gap={0.5}>
                <Heading level={2}>Approved info</Heading>
                <Text type="supporting">
                  The facts agents may quote. Only a person can approve a version, and a draft that quoted an older one cannot be sent.
                </Text>
              </VStack>
              {info.isPending ? (
                <Spinner />
              ) : info.error ? (
                <Banner status="error" title="Approved info could not be loaded." />
              ) : (
                <>
                  {draft && (
                    <Banner
                      status="warning"
                      title={`Draft v${draft.version} is waiting for approval`}
                      description={describeChange(live?.content, draft.content) + proposedBy(draft, live) + (draft.pending_quoting ? ` Approving it voids ${draft.pending_quoting} held drafts that quoted older info; they go back to the agent.` : '')}
                      collapsible={false}
                      endContent={<Button label={`Approve v${draft.version}`} variant="primary" isLoading={approve.isPending} onClick={() => approve.mutate(draft.id)} />}
                    />
                  )}
                  {live ? <InfoTable version={live} /> : <Text>No approved version yet. Agents cannot quote anything.</Text>}
                </>
              )}
            </VStack>

            <VStack gap={3}>
              <VStack gap={0.5}>
                <Heading level={2}>Permissions</Heading>
                <Text type="supporting">Defaults for a new workspace. Approval history can suggest widening one permission; it never grants one.</Text>
              </VStack>
              <Table
                data={PERMISSIONS}
                idKey="id"
                density="balanced"
                columns={[
                  {key: 'action', header: 'Action', width: proportional(2, {minWidth: 220}), renderCell: p => <Text>{p.action}</Text>},
                  {key: 'context', header: 'Context', width: proportional(1, {minWidth: 140}), renderCell: p => <Text type="supporting">{p.context}</Text>},
                  {
                    key: 'treatment',
                    header: 'Treatment',
                    width: pixel(130),
                    renderCell: p => <Token size="sm" color={p.treatment === 'Allow' ? 'green' : p.treatment === 'Hold' ? 'blue' : 'red'} label={p.treatment} />,
                  },
                  {key: 'setBy', header: 'Set by', width: pixel(100), renderCell: p => <Text type="supporting">{p.setBy}</Text>},
                ]}
              />
            </VStack>

            {versions.length > 0 && (
              <VStack gap={2}>
                <Heading level={3}>Version history</Heading>
                {versions.map(v => (
                  <HStack key={v.id} gap={2} vAlign="center" wrap="wrap">
                    <Text weight="semibold">v{v.version}</Text>
                    <StackItem size="fill">
                      <Text type="supporting">
                        {v.approved_at ? (
                          <>
                            Approved by {v.approved_by} · <Timestamp value={v.approved_at} format="auto" />
                          </>
                        ) : (
                          'Draft, not approved'
                        )}
                      </Text>
                    </StackItem>
                    {v.id === live?.id && <Token size="sm" color="green" label="Live" />}
                  </HStack>
                ))}
              </VStack>
            )}
          </VStack>
        </LayoutContent>
      }
    />
  );
}

function InfoTable({version}: {version: ApprovedInfoVersion}) {
  const c = version.content;
  const articles = (c.articles ?? []).map((a, i) => ({...a, id: `${i}:${a.title}`}));
  const sections = Object.entries(c)
    .filter(([k]) => k !== 'articles')
    .map(([key, value]) => ({id: key, key, value: summarize(value)}));
  return (
    <VStack gap={3}>
      <HStack gap={2} vAlign="center">
        <Token color="green" label={`v${version.version} is live`} />
        {version.approved_at && (
          <Text type="supporting">
            approved by {version.approved_by} · <Timestamp value={version.approved_at} format="auto" />
          </Text>
        )}
      </HStack>
      {articles.length > 0 && (
        <Table
          data={articles}
          idKey="id"
          density="balanced"
          columns={[
            {key: 'title', header: 'Article', width: proportional(1, {minWidth: 160}), renderCell: a => <Text weight="semibold">{a.title}</Text>},
            {key: 'content', header: 'Says', width: proportional(3, {minWidth: 220}), renderCell: a => <Text type="supporting">{clip(a.content)}</Text>},
            {key: 'channels', header: 'Channels', width: pixel(140), renderCell: a => <Text type="supporting">{a.channels?.length ? a.channels.join(', ') : 'All'}</Text>},
          ]}
        />
      )}
      {sections.length > 0 && (
        <Table
          data={sections}
          idKey="id"
          density="balanced"
          columns={[
            {key: 'key', header: 'Section', width: proportional(1, {minWidth: 160}), renderCell: r => <Text weight="semibold">{r.key}</Text>},
            {key: 'value', header: 'Value', width: proportional(3, {minWidth: 220}), renderCell: r => <Text type="supporting">{r.value}</Text>},
          ]}
        />
      )}
      {articles.length === 0 && sections.length === 0 && <Text type="supporting">This version is empty.</Text>}
    </VStack>
  );
}

function clip(text: string, max = 160): string {
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

// summarize shows a section's value in one line: text as is, a list of
// plain values joined, anything else as compact JSON.
function summarize(value: unknown): string {
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return clip(String(value));
  }
  if (Array.isArray(value) && value.every(v => typeof v !== 'object' || v === null)) {
    return clip(value.join(', '));
  }
  return clip(JSON.stringify(value));
}

function describeChange(from: ApprovedInfoContent | undefined, to: ApprovedInfoContent): string {
  if (!from) {
    return 'The first version of approved info.';
  }
  const parts: string[] = [];
  // Sections by key, and articles by title.
  const a = from as Record<string, unknown>;
  const b = to as Record<string, unknown>;
  const same = (x: unknown, y: unknown) => JSON.stringify(x) === JSON.stringify(y);
  const sections = [...new Set([...Object.keys(a), ...Object.keys(b)])].filter(k => k !== 'articles' && !same(a[k], b[k]));
  if (sections.length) {
    parts.push(`Changed: ${sections.join(', ')}.`);
  }
  type Article = {title?: string; content?: string};
  const titles = (v: unknown) => new Map(((Array.isArray(v) ? v : []) as Article[]).map(x => [String(x.title ?? '').toLowerCase(), x]));
  const before = titles(a.articles);
  const after = titles(b.articles);
  const added = [...after].filter(([k]) => !before.has(k)).map(([, x]) => x.title);
  const removed = [...before].filter(([k]) => !after.has(k)).map(([, x]) => x.title);
  const edited = [...after].filter(([k, x]) => before.has(k) && !same(before.get(k), x)).map(([, x]) => x.title);
  if (added.length) parts.push(`New articles: ${added.join(', ')}.`);
  if (edited.length) parts.push(`Edited articles: ${edited.join(', ')}.`);
  if (removed.length) parts.push(`Removed articles: ${removed.join(', ')}.`);
  return parts.length ? parts.join(' ') : 'No visible change.';
}

// proposedBy says which agent proposed a draft, why, and whether a newer
// version has been approved since (then it can no longer be approved).
function proposedBy(draft: ApprovedInfoVersion, live: ApprovedInfoVersion | undefined): string {
  if (!draft.proposed_by) {
    return '';
  }
  const stale = draft.based_on !== undefined && live !== undefined && live.version > draft.based_on;
  return ` Proposed by ${draft.proposed_by}${draft.reason ? `: “${draft.reason}”` : '.'}` +
    (stale ? ` It was made from v${draft.based_on}, and v${live.version} has been approved since, so it can no longer be approved; ask for it again.` : '');
}
