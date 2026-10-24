import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The bundle is embedded by internal/webui, so the output directory lives
// outside the web root. base "/" keeps asset URLs absolute for the SPA host.
export default defineConfig({
  base: '/',
  plugins: [vue()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      // The admin API only accepts same-origin requests carrying the host's
      // session cookie, so the dev proxy has to look like the host itself:
      // rewrite Origin to the target and serve the SPA document through the
      // same proxy so the cookie is issued by the host.
      '/api': {
        target: 'http://127.0.0.1:7865',
        changeOrigin: true,
        rewriteRequestHeaders: (_req, headers) => {
          delete headers.origin
          delete headers.referer
          return headers
        },
      },
    },
  },
})
