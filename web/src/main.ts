/**
 * 应用入口。
 * Pinia 必须在使用任何 store 之前安装（app.use(pinia) 会设置 activePinia，
 * 之后在组件外调用 useXxxStore() 才能拿到同一个实例）。
 */

import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
// KaTeX 样式（数学公式渲染，见 utils/markdown.ts）；字体按需加载，带 hash 进 /assets
import 'katex/dist/katex.min.css'
import './styles/main.css'

const app = createApp(App)
app.use(createPinia())
app.mount('#app')

// PWA：注册 Service Worker。失败只告警降级，不影响主功能。
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch((err) => {
      console.warn('[PWA] Service Worker 注册失败，已降级为普通网页模式:', err)
    })
  })
}
