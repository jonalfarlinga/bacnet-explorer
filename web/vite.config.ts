import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// "npm run dev" serves the UI with hot reload and passes API calls on to an
// explorer running on its default address.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: { '/api': 'http://127.0.0.1:8080' },
  },
})
