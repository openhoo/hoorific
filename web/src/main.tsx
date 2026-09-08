import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './app';
import './styles.css';

const root = document.getElementById('app');

if (!root) {
  throw new Error('Hoorific console root element #app is missing.');
}

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>
);
