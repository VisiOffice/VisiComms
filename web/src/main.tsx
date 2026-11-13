import '@astryxdesign/core/reset.css';
import '@astryxdesign/core/astryx.css';
import '@astryxdesign/theme-neutral/theme.css';

import {StrictMode, forwardRef, type AnchorHTMLAttributes} from 'react';
import {createRoot} from 'react-dom/client';
import {BrowserRouter, Link as RouterLink} from 'react-router-dom';
import {QueryClient, QueryClientProvider} from '@tanstack/react-query';
import {Theme} from '@astryxdesign/core/theme';
import {LinkProvider} from '@astryxdesign/core/Link';
import {ToastViewport} from '@astryxdesign/core/Toast';
import {neutralTheme} from '@astryxdesign/theme-neutral/built';

import {App} from './App';
import {ApiError} from './api';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      // A 401 or 404 will not fix itself on retry.
      retry: (count, err) => !(err instanceof ApiError && err.status < 500) && count < 2,
    },
  },
});

// Astryx links (Link, Button with href, nav items) navigate through the
// router instead of reloading the page; absolute URLs stay plain anchors.
const AppLink = forwardRef<HTMLAnchorElement, AnchorHTMLAttributes<HTMLAnchorElement>>(({href = '', ...rest}, ref) =>
  /^(https?:|mailto:|tel:)/.test(href) ? <a ref={ref} href={href} {...rest} /> : <RouterLink ref={ref} to={href} {...rest} />,
);

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <Theme theme={neutralTheme}>
          <LinkProvider component={AppLink}>
            <ToastViewport>
              <App />
            </ToastViewport>
          </LinkProvider>
        </Theme>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
