import type {CSSProperties, ReactNode} from 'react';
import {VStack} from '@astryxdesign/core/Layout';
import {Center} from '@astryxdesign/core/Center';
import {Text, Heading} from '@astryxdesign/core/Text';
import {Card} from '@astryxdesign/core/Card';
import {Icon} from '@astryxdesign/core/Icon';
import {ChatBubbleLeftRightIcon} from '@heroicons/react/24/outline';

// From the login-card template: a standalone page paints its own background
// and caps the column at 400px while letting it shrink on narrow screens.
const pageStyle: CSSProperties = {minHeight: '100dvh', backgroundColor: 'var(--color-background-body)'};
const contentStyle: CSSProperties = {width: '100%', maxWidth: 400};

export function AuthFrame({title, subtitle, children, footer}: {title: string; subtitle: string; children: ReactNode; footer?: ReactNode}) {
  return (
    <Center axis="both" padding={6} style={pageStyle}>
      <VStack gap={4} hAlign="center" style={contentStyle}>
        <VStack gap={2} hAlign="center">
          <Icon icon={ChatBubbleLeftRightIcon} size="lg" />
          <Text type="body" weight="bold" size="lg">
            VisiComms
          </Text>
        </VStack>
        <Card padding={8} width="100%">
          <VStack gap={4} hAlign="stretch">
            <VStack gap={1} hAlign="center">
              <Heading level={1} justify="center">
                {title}
              </Heading>
              <Text type="body" color="secondary" size="sm" justify="center">
                {subtitle}
              </Text>
            </VStack>
            {children}
          </VStack>
        </Card>
        {footer}
      </VStack>
    </Center>
  );
}
