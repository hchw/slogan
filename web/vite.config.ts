/// <reference types="vitest" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/user/api/v1': 'http://127.0.0.1:8080',
      '/admin/api/v1': 'http://127.0.0.1:8080',
      '/v1': { target: 'http://127.0.0.1:8081', changeOrigin: true },
    },
  },
  test: { environment: 'jsdom', setupFiles: './src/test-setup.ts' },
});
