import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';

export default defineConfig({
  base: '/admin/',
  build: {
    outDir: '../internal/console/assets',
    emptyOutDir: true,
    rollupOptions: { input: 'index.html' }
  },
  server: { port: 5173 }
});
