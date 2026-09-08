import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  base: '/admin/',
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': new URL('./src', import.meta.url).pathname } },
  build: {
    outDir: '../internal/console/assets',
    emptyOutDir: true,
    rollupOptions: { input: 'index.html' }
  },
  server: { port: 5173 }
});
