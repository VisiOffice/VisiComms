import {useCallback, useLayoutEffect, useMemo, useRef, useState} from 'react';
import {useNavigate} from 'react-router-dom';
import {useQuery} from '@tanstack/react-query';
import {HStack, Layout, LayoutContent, LayoutHeader, LayoutPanel, StackItem, VStack} from '@astryxdesign/core/Layout';
import {Heading, Text} from '@astryxdesign/core/Text';
import {Badge} from '@astryxdesign/core/Badge';
import {Banner} from '@astryxdesign/core/Banner';
import {Button} from '@astryxdesign/core/Button';
import {EmptyState} from '@astryxdesign/core/EmptyState';
import {Icon} from '@astryxdesign/core/Icon';
import {ResizeHandle, useResizable} from '@astryxdesign/core/Resizable';
import {Spinner} from '@astryxdesign/core/Spinner';
import {Tab, TabList} from '@astryxdesign/core/TabList';
import {Token} from '@astryxdesign/core/Token';
import {VisuallyHidden} from '@astryxdesign/core/VisuallyHidden';
import {Table, pixel, proportional} from '@astryxdesign/core/Table';
import type {TableColumn, TablePlugin} from '@astryxdesign/core/Table';
import {ArrowPathIcon, CheckCircleIcon} from '@heroicons/react/24/outline';

import {api, type AttentionItem, type AttentionKind, type SetupStatus} from '../api';
import {useMe} from '../App';
import {SetupChecklist} from '../components/SetupChecklist';
import {ThreadPane} from '../components/ThreadPane';
import {dueLabel, waited} from '../format';

type Filter = 'all' | AttentionKind;

const KIND: Record<AttentionKind, {label: string; plural: string; color: 'orange' | 'blue' | 'red' | 'gray'}> = {
  held_draft: {label: 'Held draft', plural: 'Held drafts', color: 'blue'},
  unanswered: {label: 'Unanswered', plural: 'Unanswered', color: 'orange'},
  failed_run: {label: 'Failed run', plural: 'Failed runs', color: 'red'},
  unknown_send: {label: 'Unknown send', plural: 'Unknown sends', color: 'red'},
  send_failed: {label: 'Not delivered', plural: 'Not delivered', color: 'red'},
  complaint: {label: 'Spam complaint', plural: 'Complaints', color: 'red'},
  connector_error: {label: 'Connector error', plural: 'Connector errors', color: 'red'},
};

// From the table-inbox template: below this the queue and the pane cannot sit
// side by side, so opening an item replaces the list instead.
const SINGLE_SURFACE_BELOW = 900;
const STACK_ROWS_BELOW = 520;
const PANE_MIN_WIDTH = 440;
const LIST_MIN_WIDTH = 320;
const OPEN_CELL_STYLE = {backgroundColor: 'var(--color-accent-muted)'};
const INTERACTIVE_WITHIN_ROW = 'button, input, a, label, [role="button"]';

function useWidth() {
  const ref = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const node = ref.current;
    if (!node) {
      return;
    }
    setWidth(node.getBoundingClientRect().width);
    const observer = new ResizeObserver(entries => setWidth(entries[0]?.contentRect.width ?? 0));
    observer.observe(node);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

export function AttentionPage() {
  const navigate = useNavigate();
  const [filter, setFilter] = useState<Filter>('all');
  const [chosen, setChosen] = useState<string | null | undefined>(undefined);
  const attention = useQuery({
    queryKey: ['attention'],
    queryFn: () => api.get<{items: AttentionItem[]}>('/attention'),
    refetchInterval: 30_000,
  });
  const items = useMemo(() => attention.data?.items ?? [], [attention.data]);
  const setup = useQuery({queryKey: ['setup'], queryFn: () => api.get<SetupStatus>('/setup')});
  const me = useMe();
  const visible = useMemo(() => (filter === 'all' ? items : items.filter(i => i.kind === filter)), [items, filter]);

  const [surfaceRef, surfaceWidth] = useWidth();
  const [listRef, listWidth] = useWidth();
  const isMeasured = surfaceWidth > 0;
  const isSingleSurface = isMeasured && surfaceWidth < SINGLE_SURFACE_BELOW;
  const isStacked = listWidth > 0 && listWidth < STACK_ROWS_BELOW;

  // The pane shows the chosen thread, or the top of the queue on a wide screen
  // so the first thing waiting is already open. Closing it keeps it closed.
  const withThread = visible.filter(i => i.thread_id);
  const openId =
    chosen === null
      ? null
      : chosen !== undefined && withThread.some(i => i.thread_id === chosen)
        ? chosen
        : isSingleSurface
          ? null
          : (withThread[0]?.thread_id ?? null);

  const pane = useResizable({
    defaultSize: '50%',
    minSize: PANE_MIN_WIDTH,
    maxSize: isMeasured ? Math.max(PANE_MIN_WIDTH, surfaceWidth - LIST_MIN_WIDTH) : undefined,
  });

  const open = useCallback(
    (item: AttentionItem) => {
      if (!item.thread_id) {
        navigate('/connections');
        return;
      }
      if (isSingleSurface) {
        navigate(`/threads/${item.thread_id}`);
        return;
      }
      setChosen(item.thread_id);
    },
    [isSingleSurface, navigate],
  );

  const rows = useMemo(
    (): TablePlugin<AttentionItem> => ({
      transformBodyRow: (props, item) => ({
        ...props,
        htmlProps: {
          ...props.htmlProps,
          tabIndex: 0,
          style: {...props.htmlProps.style, cursor: 'pointer'},
          'aria-current': item.thread_id === openId ? true : undefined,
          onClick: event => {
            if (!(event.target as HTMLElement).closest(INTERACTIVE_WITHIN_ROW)) {
              open(item);
            }
          },
          onKeyDown: event => {
            if (event.key === 'Enter' || event.key === ' ') {
              event.preventDefault();
              open(item);
            }
          },
        },
      }),
      transformBodyCell: (props, _column, item) =>
        item.thread_id && item.thread_id === openId
          ? {...props, htmlProps: {...props.htmlProps, style: {...props.htmlProps.style, ...OPEN_CELL_STYLE}}}
          : props,
    }),
    [open, openId],
  );

  const columns = useMemo((): TableColumn<AttentionItem>[] => {
    const timing: TableColumn<AttentionItem> = {
      key: 'timing',
      header: 'Waiting',
      width: pixel(124),
      align: 'end',
      renderCell: item => <Timing item={item} />,
    };
    if (isStacked) {
      return [
        {
          key: 'item',
          header: 'Needs you',
          width: proportional(1, {minWidth: 160}),
          renderCell: item => (
            <VStack gap={0.5}>
              <HStack gap={2} vAlign="center">
                <Token size="sm" color={KIND[item.kind].color} label={KIND[item.kind].label} />
                <Text maxLines={1} weight="semibold">
                  {item.contact || item.subject}
                </Text>
              </HStack>
              <Text maxLines={1}>{item.subject}</Text>
              <Text maxLines={1} type="supporting">
                {item.reason}
              </Text>
            </VStack>
          ),
        },
        timing,
      ];
    }
    return [
      {
        key: 'kind',
        header: 'Status',
        width: pixel(140),
        renderCell: item => <Token size="sm" color={KIND[item.kind].color} label={KIND[item.kind].label} />,
      },
      {
        key: 'contact',
        header: 'From',
        width: pixel(160),
        renderCell: item => (
          <Text maxLines={1} weight="semibold">
            {item.contact || '—'}
          </Text>
        ),
      },
      {
        key: 'subject',
        header: 'What',
        width: proportional(1, {minWidth: 200}),
        renderCell: item => (
          <VStack gap={0.5}>
            <Text maxLines={1}>{item.subject}</Text>
            <Text maxLines={1} type="supporting">
              {item.reason}
            </Text>
          </VStack>
        ),
      },
      timing,
    ];
  }, [isStacked]);

  const longest = items.reduce<string | null>((oldest, i) => (!oldest || i.waiting_since < oldest ? i.waiting_since : oldest), null);
  const showPane = !isSingleSurface && openId !== null;

  return (
    <Layout
      ref={surfaceRef}
      height="fill"
      header={
        <LayoutHeader padding={0}>
          <VStack gap={0}>
            <HStack gap={3} vAlign="center" wrap="wrap" padding={4}>
              <StackItem size="fill">
                <VStack gap={0.5}>
                  <Heading level={1}>Needs attention</Heading>
                  <Text type="supporting">
                    {items.length === 0
                      ? 'Nothing is waiting on you'
                      : `${items.length} ${items.length === 1 ? 'item' : 'items'}${longest ? ` · longest wait ${waited(longest)}` : ''} · most urgent first`}
                  </Text>
                </VStack>
              </StackItem>
              <Button
                label="Refresh"
                variant="ghost"
                icon={<Icon icon={ArrowPathIcon} size="sm" />}
                isLoading={attention.isFetching}
                onClick={() => attention.refetch()}
              />
            </HStack>
            <VStack paddingInline={1}>
              <TabList hasDivider value={filter} onChange={value => setFilter(value as Filter)}>
                <Tab value="all" label="All" />
                {(Object.keys(KIND) as AttentionKind[]).map(kind => {
                  const n = items.filter(i => i.kind === kind).length;
                  return n === 0 ? null : (
                    <Tab
                      key={kind}
                      value={kind}
                      label={KIND[kind].plural}
                      endContent={
                        <Badge
                          variant="neutral"
                          label={
                            <>
                              {n}
                              <VisuallyHidden> waiting</VisuallyHidden>
                            </>
                          }
                        />
                      }
                    />
                  );
                })}
              </TabList>
            </VStack>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent ref={listRef} padding={4} isScrollable>
          {attention.isPending ? (
            <VStack padding={6} hAlign="center">
              <Spinner />
            </VStack>
          ) : attention.error ? (
            <Banner status="error" title="The queue could not be loaded." description={attention.error.message} />
          ) : items.length === 0 && setup.data && !setup.data.complete ? (
            // An empty queue in a workspace that can't receive or send mail
            // is not "all clear"; say what is missing instead.
            <VStack padding={2} hAlign="center">
              <SetupChecklist status={setup.data} workspace={me.data?.workspace.name ?? 'your workspace'} />
            </VStack>
          ) : visible.length === 0 ? (
            <VStack padding={6}>
              <EmptyState
                title="All clear"
                description={filter === 'all' ? 'Nothing is waiting on a person. Agents are handling the rest.' : 'Nothing of this kind is waiting.'}
                icon={<Icon icon={CheckCircleIcon} size="lg" />}
                actions={filter === 'all' ? undefined : <Button label="Show everything" onClick={() => setFilter('all')} />}
              />
            </VStack>
          ) : (
            <Table data={visible} columns={columns} idKey="id" plugins={{rows}} density="balanced" dividers="rows" hasHover textOverflow="truncate" />
          )}
        </LayoutContent>
      }
      end={
        showPane && openId ? (
          <>
            <ResizeHandle direction="horizontal" isReversed hasDivider pillPlacement="end" resizable={pane.props} label="Resize thread pane" />
            <LayoutPanel resizable={pane.props} padding={0} role="complementary" label="Thread">
              <ThreadPane threadId={openId} onClose={() => setChosen(null)} isFullWidth={false} />
            </LayoutPanel>
          </>
        ) : undefined
      }
    />
  );
}

function Timing({item}: {item: AttentionItem}) {
  const due = dueLabel(item.due_at);
  return (
    <VStack gap={0.5} hAlign="end">
      <Text hasTabularNumbers>{waited(item.waiting_since)}</Text>
      {due ? (
        <Text type="supporting" weight={due.isLate || due.isSoon ? 'semibold' : undefined} color={due.isLate ? 'accent' : 'secondary'}>
          {due.text}
        </Text>
      ) : (
        <Text type="supporting">{item.kind === 'unknown_send' ? 'Not retried' : item.kind === 'send_failed' ? 'Check address' : ''}</Text>
      )}
    </VStack>
  );
}
