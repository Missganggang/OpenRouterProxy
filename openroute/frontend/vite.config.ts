import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'

// 构建产物直接输出到后端的 html-path（../public），
// 这样单个 Go 二进制即可同时提供 API 与 WebUI（规格书 9.4）。
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  build: {
    outDir: '../public',
    emptyOutDir: true,
    // 单个 chunk 保持在 1MB 以下，避免首屏加载过慢。
    chunkSizeWarningLimit: 1000,
    rollupOptions: {
      output: {
        manualChunks: {
          react: ['react', 'react-dom', 'react-router-dom'],
          antd: ['antd', '@ant-design/icons'],
          charts: ['@ant-design/plots'],
        },
      },
    },
  },
  server: {
    port: 5173,
    // 开发时把 API 请求代理到后端，避免跨域与 Cookie 问题。
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:18888',
        changeOrigin: true,
      },
      '/sub': {
        target: 'http://127.0.0.1:18888',
        changeOrigin: true,
      },
    },
  },
})
