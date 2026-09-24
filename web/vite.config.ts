import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

// 前端源码在 web/，构建产物直接输出到 Go 的 embed 目录（internal/webui/dist），
// 这样 `go build` 就能把整个前端打进二进制，部署仍然只有一个文件。
export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    // 开发时把 /api 代理到本地跑着的 zenofs，省得处理跨域
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
    },
  },
})
