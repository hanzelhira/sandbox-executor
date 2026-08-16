import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Dev-server proxy: the browser talks to Vite on :5173, and Vite forwards
// API calls to the Go server on :8080. Same-origin from the browser's view,
// so no CORS anywhere. `ws: true` upgrades /stream connections.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/execute': 'http://localhost:8080',
      '/stats': 'http://localhost:8080',
      '/config': 'http://localhost:8080',
      '/sessions': 'http://localhost:8080',
      '/stream': {
        target: 'ws://localhost:8080',
        ws: true,
      },
    },
  },
})
