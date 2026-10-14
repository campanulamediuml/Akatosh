import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 后端固定监听 127.0.0.1:28888，开发期用 proxy 规避 CORS。
export default defineConfig({
  plugins: [vue()],
  // 相对 base，便于后端 go:embed 后从任意路径挂载。
  base: './',
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:28888',
        changeOrigin: true,
        // SSE 必须关闭代理层缓冲，否则增量会被攒着一起发。
        ws: false,
        configure: (proxy) => {
          proxy.on('proxyRes', (proxyRes) => {
            if (String(proxyRes.headers['content-type'] || '').includes('text/event-stream')) {
              proxyRes.headers['x-accel-buffering'] = 'no'
              delete proxyRes.headers['content-length']
            }
          })
        },
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 1500,
  },
})
