import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// dev server 把后端路径转给面板进程。默认面板在本机 8080；该端口被别的进程
// 占用时用 NFT_PANEL_URL 指到实际地址（例：NFT_PANEL_URL=http://127.0.0.1:18080 npm run dev）。
// /api 下有 WebSocket（/api/ws/speed），所以必须开 ws 转发，否则 dev 下速度列永远是空的。
//
// 只转发面板真正注册的后端前缀（server.go：/api、/v1、/healthz）。/login 是前端
// 路由：把它转给面板会拿回面板内嵌的旧构建产物，dev 下登录页就不再是当前源码。
const panel = process.env.NFT_PANEL_URL || 'http://localhost:8080'
const toPanel = { target: panel, ws: true }

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': toPanel,
      '/v1': toPanel,
      '/healthz': panel,
    },
  },
})
