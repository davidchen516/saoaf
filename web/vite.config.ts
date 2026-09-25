import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // Strip the /api prefix: the control plane exposes probes at the root
      // (/healthz, /readyz), so /api/healthz must reach :8080/healthz.
      // SAOAF_API_TARGET overrides the dev proxy for E2E stacks.
      '/api': {
        target: process.env.SAOAF_API_TARGET ?? 'http://127.0.0.1:8080',
        rewrite: (path) => path.replace(/^\/api/, ''),
      },
    },
  },
})
