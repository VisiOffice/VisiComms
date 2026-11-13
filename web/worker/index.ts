// Serves the console and forwards /api to the Go backend, so the browser
// talks to one origin: the session cookie stays first-party and no CORS is
// needed. Static files and client-side routes are served by the assets
// binding; only /api/* reaches this code (run_worker_first in wrangler.jsonc).

interface Env {
  ASSETS: Fetcher;
  // https://api.visicomms.com
  API_ORIGIN: string;
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    if (!url.pathname.startsWith('/api/')) {
      return env.ASSETS.fetch(request);
    }
    const upstream = new URL(url.pathname + url.search, env.API_ORIGIN);
    const headers = new Headers(request.headers);
    headers.set('X-Forwarded-Host', url.host);
    headers.set('X-Forwarded-Proto', url.protocol.replace(':', ''));
    return fetch(upstream, {
      method: request.method,
      headers,
      body: request.method === 'GET' || request.method === 'HEAD' ? undefined : request.body,
      redirect: 'manual',
    });
  },
};
