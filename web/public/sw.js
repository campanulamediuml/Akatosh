/* Akatosh Service Worker — 手写，无依赖。
   策略：
   - /api/*           ：一律放行（network-only），SSE 长连接绝不进缓存
   - /assets/*        ：hash 命名、immutable，cache-first
   - 导航请求          ：network-first，成功即刷新缓存的 /index.html（滚动到最新），
                        离线回退缓存的 /index.html（SPA 离线壳）
   - 其余 GET         ：network-first，成功写缓存
   非 GET 或带 Authorization 的请求一律不拦截。

   __SW_BUILD__ 由构建脚本（web/scripts/inject-sw-version.mjs）替换成当次构建的
   内容 hash —— 每次前端变更后 sw.js 字节必然变化，浏览器会触发 SW 更新：
   install（预缓存新入口）→ activate（按缓存名清掉上一版缓存）。 */

const VERSION = '__SW_BUILD__'
const CACHE = `ai-assistant-${VERSION}`
const PRECACHE = ['/', '/index.html']

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE).then((cache) => cache.addAll(PRECACHE)).then(() => self.skipWaiting())
  )
})

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  )
})

self.addEventListener('fetch', (event) => {
  const req = event.request

  // 只处理 GET；带 Authorization 的请求不拦截
  if (req.method !== 'GET' || req.headers.has('authorization')) return

  const url = new URL(req.url)

  // 只处理同源请求
  if (url.origin !== self.location.origin) return

  // API 一律 network-only（/api/stream 是 SSE 长连接，绝不能缓存）
  if (url.pathname.startsWith('/api/')) return

  // hash 命名的静态资源：cache-first
  if (url.pathname.startsWith('/assets/')) {
    event.respondWith(
      caches.match(req).then(
        (cached) =>
          cached ||
          fetch(req).then((resp) => {
            if (resp.ok) {
              const clone = resp.clone()
              caches.open(CACHE).then((cache) => cache.put(req, clone))
            }
            return resp
          })
      )
    )
    return
  }

  // 导航请求：network-first，失败回退缓存的 index.html（SPA 离线壳）
  if (req.mode === 'navigate') {
    event.respondWith(
      fetch(req)
        .then((resp) => {
          if (resp.ok) {
            const clone = resp.clone()
            caches.open(CACHE).then((cache) => cache.put('/index.html', clone))
          }
          return resp
        })
        .catch(() =>
          caches.match(req).then((cached) => cached || caches.match('/index.html'))
        )
    )
    return
  }

  // 其余 GET：network-first，成功写缓存，失败回退缓存
  event.respondWith(
    fetch(req)
      .then((resp) => {
        if (resp.ok) {
          const clone = resp.clone()
          caches.open(CACHE).then((cache) => cache.put(req, clone))
        }
        return resp
      })
      .catch(() => caches.match(req))
  )
})
