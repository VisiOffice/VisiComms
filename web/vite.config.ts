import {defineConfig} from 'vite';
import react from '@vitejs/plugin-react';

// In development the Go API runs on :8080 and Vite forwards /api to it, so
// the page and the API share one origin, as they do behind the Cloudflare
// Worker in production (worker/index.ts).
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {'/api': 'http://localhost:8080'},
  },
});
