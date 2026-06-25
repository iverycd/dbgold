import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'path'

export default defineConfig(({ mode }) => {
  // 读取 frontend/.env* 中的变量（第三个参数 '' 表示不限制 VITE_ 前缀也能读，但这里仍用 VITE_ 前缀）
  const env = loadEnv(mode, __dirname, '')
  const apiTarget = env.VITE_API_TARGET || 'http://localhost:8080'
  return {
    plugins: [vue()],
    resolve: {
      alias: {
        '@': resolve(__dirname, 'src'),
      },
    },
    server: {
      port: 5173,
      proxy: {
        '/api': {
          target: apiTarget,
          changeOrigin: true,
        },
      },
    },
  }
})
