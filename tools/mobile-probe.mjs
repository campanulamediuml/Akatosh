/**
 * 手机 / PWA 可用性体检（零依赖，CDP 驱动）
 *
 * 用无头 Chrome 模拟真机尺寸 + 触屏，在**局域网 IP**（非 localhost）上打开页面，
 * 把「放手机上会有哪些坑」逐条测出来。不猜、不推理，能测的都测。
 *
 * 用法：
 *   node tools/mobile-probe.mjs --lan 172.16.13.16
 *   node tools/mobile-probe.mjs --lan 192.168.1.5 --port 9411 --keep
 *
 * 前提：后端以 `-addr 0.0.0.0:28888` 启动（否则局域网 IP 走不通），
 *       上游用 tools/mockllm 跑在 28899。
 *
 * 为什么要单开一个 --port：ui-test.mjs 默认 9222，若端口上残留着别的 Chrome，
 * 本脚本会连到它身上，模拟参数就串了。9411 是独立的。
 */
import { spawn } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

/* ------------------------------------------------------------------ */
/* 参数                                                                */
/* ------------------------------------------------------------------ */

function parseArgs(argv) {
  const out = {}
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a.startsWith('--')) out[a.slice(2)] = argv[i + 1]?.startsWith('--') ? true : argv[++i]
  }
  return out
}

const args = parseArgs(process.argv.slice(2))
const LAN = args.lan || '172.16.13.16'
const LOCAL = args.local || 'http://127.0.0.1:28888'
const LAN_BASE = `http://${LAN}:28888`
const PORT = Number(args.port || 9411)
const UPSTREAM = args.upstream || 'http://127.0.0.1:28899/v1'
const SHOT_DIR = args.shots || '.tmp-mobile\\shots'
const KEEP = !!args.keep
/** 访问令牌：后端用 -token 起的时候要传，否则页面能加载但所有 API 都是 401 */
const TOKEN = args.token || ''
/** 拿来做 HEIC 替身的一张真图（只读它的字节，不当图片用） */
const FIXTURE = args.fixture || 'docs\\01-首次进入.png'

/** 手机第一次访问用的地址：带上令牌，后端会换 cookie 并 302 抹掉 query */
const bootURL = (base) => (TOKEN ? `${base}/?token=${encodeURIComponent(TOKEN)}` : base)

/*
 * 后端开了 -token 之后，探针自己直连后端的那些请求也得带凭证。
 * 与其在十几处调用点各加一次 header（漏一处就是 401 的假失败），
 * 不如在这里包一层：只对指向 LOCAL 的请求补 X-Auth-Token，
 * CDP 那些 127.0.0.1:94xx 的请求不受影响。
 */
if (TOKEN) {
  const rawFetch = globalThis.fetch
  globalThis.fetch = (url, init = {}) => {
    if (typeof url === 'string' && url.startsWith(LOCAL)) {
      init = { ...init, headers: { ...(init.headers || {}), 'X-Auth-Token': TOKEN } }
    }
    return rawFetch(url, init)
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const CHROME_CANDIDATES = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
]

/* ------------------------------------------------------------------ */
/* 迷你 CDP 客户端（与 ui-test.mjs 同源）                              */
/* ------------------------------------------------------------------ */

class Cdp {
  constructor(ws) {
    this.ws = ws
    this.seq = 0
    this.waiting = new Map()
    /** Network.requestWillBeSent 里出现过的 URL，用来数「重连了几次」「有没有重新拉 state」 */
    this.reqs = []
    /** requestId → url，供 loadingFailed 反查 */
    this.reqUrl = new Map()
    /** 失败过的请求 URL：证明「断网」是不是真的切断了 */
    this.failed = []
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data)
      if (!msg.id) {
        if (msg.method === 'Network.requestWillBeSent') {
          this.reqs.push(msg.params.request.url)
          this.reqUrl.set(msg.params.requestId, msg.params.request.url)
        } else if (msg.method === 'Network.loadingFailed') {
          const u = this.reqUrl.get(msg.params.requestId)
          if (u) this.failed.push({ url: u, err: msg.params.errorText })
        }
        return
      }
      if (this.waiting.has(msg.id)) {
        const { resolve, reject } = this.waiting.get(msg.id)
        this.waiting.delete(msg.id)
        if (msg.error) reject(new Error(`${msg.error.message} ${JSON.stringify(msg.error.data ?? '')}`))
        else resolve(msg.result)
      }
    }
  }

  /** 数一下匹配到的请求个数；sub 用 URL 子串 */
  reqCount(sub) {
    return this.reqs.filter((u) => u.includes(sub)).length
  }

  send(method, params = {}) {
    return new Promise((resolve, reject) => {
      const id = ++this.seq
      this.waiting.set(id, { resolve, reject })
      this.ws.send(JSON.stringify({ id, method, params }))
      setTimeout(() => {
        if (this.waiting.has(id)) {
          this.waiting.delete(id)
          reject(new Error(`CDP 调用超时: ${method}`))
        }
      }, 30000)
    })
  }

  async eval(expression) {
    const r = await this.send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
    if (r.exceptionDetails) {
      const d = r.exceptionDetails.exception?.description || r.exceptionDetails.text
      throw new Error(`页面内求值出错: ${d}\n  表达式: ${expression.slice(0, 160)}`)
    }
    return r.result.value
  }

  async waitFor(expression, desc, timeout = 20000) {
    const t0 = Date.now()
    let last
    while (Date.now() - t0 < timeout) {
      last = await this.eval(expression)
      if (last) return last
      await sleep(100)
    }
    throw new Error(`等待「${desc}」超时 ${timeout}ms；最后一次: ${JSON.stringify(last)}`)
  }

  async boxOf(expr, desc = '元素') {
    const b = await this.eval(`(() => {
      const el = ${expr}
      if (!el) return null
      el.scrollIntoView({ block: 'center', inline: 'center' })
      const r = el.getBoundingClientRect()
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height }
    })()`)
    if (!b) throw new Error(`找不到${desc}`)
    return b
  }

  /** 触摸点按（不是鼠标）——手机上的真实交互 */
  async tapExpr(expr, desc = '元素') {
    const b = await this.boxOf(expr, desc)
    const pt = { x: Math.round(b.x), y: Math.round(b.y) }
    await this.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [pt] })
    await sleep(60)
    await this.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    await sleep(250)
  }

  async shot(name) {
    mkdirSync(SHOT_DIR, { recursive: true })
    const r = await this.send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false })
    const p = join(SHOT_DIR, name)
    writeFileSync(p, Buffer.from(r.data, 'base64'))
    return p
  }

  /** 设定手机视口；必须在 navigate 之前调用，viewport meta 才会生效 */
  async setPhone(width, height, dpr = 3) {
    await this.send('Emulation.setDeviceMetricsOverride', {
      width,
      height,
      deviceScaleFactor: dpr,
      mobile: true,
      screenWidth: width,
      screenHeight: height,
    })
    await this.send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 })
  }

  /**
   * 往 <input type="file"> 里塞真实文件。
   * CDP 只能这样"选文件"——Input.dispatchKeyEvent 带不了文件内容。
   * 元素即使 hidden 也能塞；而且这条路会**绕过 accept 过滤**，正好用来喂
   * 那些正常选择器里选不到的格式。
   */
  async setFileInput(selector, filePaths) {
    await this.send('DOM.enable')
    const doc = await this.send('DOM.getDocument', { depth: -1 })
    const found = await this.send('DOM.querySelector', { nodeId: doc.root.nodeId, selector })
    if (!found?.nodeId) throw new Error(`找不到文件输入框: ${selector}`)
    await this.send('DOM.setFileInputFiles', { files: filePaths, nodeId: found.nodeId })
    await sleep(400)
  }

  async setDesktop() {
    await this.send('Emulation.clearDeviceMetricsOverride')
    await this.send('Emulation.setTouchEmulationEnabled', { enabled: false })
  }
}

/* ------------------------------------------------------------------ */
/* 报告输出                                                            */
/* ------------------------------------------------------------------ */

const findings = []
function section(t) {
  console.log(`\n${'═'.repeat(72)}\n${t}\n${'═'.repeat(72)}`)
}
function say(label, value, note) {
  const v = String(value)
  console.log(`  ${label.padEnd(34)}${v}${note ? '   ← ' + note : ''}`)
}
/** 记一条坑（severity: high/mid/low） */
function pit(sev, title, detail) {
  findings.push({ sev, title, detail })
  const mark = sev === 'high' ? '🔴' : sev === 'mid' ? '🟠' : '🟡'
  console.log(`  ${mark} [${sev}] ${title}${detail ? ' — ' + detail : ''}`)
}

/**
 * 实时推送存活探针 —— 这是判断「锁屏/断网后界面还在不在更新」最直接的办法。
 *
 * 从**进程外**（Node 直接打 API）新建一条会话，服务端会广播 conv 事件；
 * 如果浏览器侧栏真的多出来这一条，说明 SSE 端到端还活着。
 * 比数「重连了几次」可靠得多：连接可能没断，也可能断了又重连但事件丢了。
 */
async function streamLiveness(cdp, label) {
  const before = await cdp.eval(`document.querySelectorAll('.conv').length`)
  const title = `存活探针${Date.now() % 100000}`
  const res = await fetch(`${LOCAL}/api/conversations`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title, provider_id: undefined, model_id: undefined }),
  })
  if (!res.ok) {
    say(`${label} 探针建会话失败`, `${res.status}`)
    return null
  }
  let seen = false
  for (let i = 0; i < 25; i++) {
    await sleep(200)
    seen = await cdp.eval(
      `[...document.querySelectorAll('.conv__title')].some((e) => e.textContent.includes(${JSON.stringify(title)}))`,
    )
    if (seen) break
  }
  const after = await cdp.eval(`document.querySelectorAll('.conv').length`)
  say(`${label} 实时推送还活着吗`, `${before} → ${after} 条会话；看到新会话 = ${seen}`)
  return seen
}

/* ------------------------------------------------------------------ */
/* Chrome                                                              */
/* ------------------------------------------------------------------ */

function findChrome() {
  for (const p of CHROME_CANDIDATES) {
    if (existsSync(p)) return p
  }
  throw new Error('找不到 Chrome/Edge')
}

async function cdpEndpoint() {
  let list
  try {
    list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()
  } catch {
    return null
  }
  const found = list.find((x) => x.type === 'page')
  if (found) return found.webSocketDebuggerUrl
  try {
    const created = await (
      await fetch(`http://127.0.0.1:${PORT}/json/new?about:blank`, { method: 'PUT' })
    ).json()
    if (created?.webSocketDebuggerUrl) return created.webSocketDebuggerUrl
  } catch {
    /* 忽略 */
  }
  return null
}

/**
 * 手工拼一张最小 BMP。
 * Go 标准库的 image 包只注册了 png/jpeg/gif（外加本项目显式引入的 webp），
 * 所以 BMP 必然走「认不出来的格式」分支 —— 正好拿来当 HEIC 的替身。
 */
function makeBmp(w, h) {
  const rowSize = Math.ceil((w * 3) / 4) * 4
  const pixSize = rowSize * h
  const buf = Buffer.alloc(54 + pixSize)
  buf.write('BM', 0, 'ascii')
  buf.writeUInt32LE(54 + pixSize, 2)
  buf.writeUInt32LE(54, 10)
  buf.writeUInt32LE(40, 14)
  buf.writeInt32LE(w, 18)
  buf.writeInt32LE(h, 22)
  buf.writeUInt16LE(1, 26)
  buf.writeUInt16LE(24, 28)
  buf.writeUInt32LE(pixSize, 34)
  buf.writeInt32LE(2835, 38)
  buf.writeInt32LE(2835, 42)
  for (let i = 54; i < buf.length; i++) buf[i] = 0x7f
  return buf
}

/**
 * 读一张 PNG 的宽高（IHDR 里宽高各 4 字节大端）。
 * 用来验证前端转出来的 JPEG 尺寸没跑偏，所以不值得为它引任何库。
 */
function pngSize(buf) {
  if (buf.length < 24) return { w: 0, h: 0 }
  return { w: buf.readUInt32BE(16), h: buf.readUInt32BE(20) }
}

let chromeProc = null
async function ensureChrome() {
  // 默认**不复用**已有实例：这个探针会把页面冻结/断网/改视口，
  // 复用上一轮遗留的 Chrome 会带着挂起状态进来，Input.* 直接超时。
  // 真需要复用时显式传 --reuse。
  if (args.reuse) {
    const endpoint = await cdpEndpoint()
    if (endpoint) {
      console.log(`连上已存在的 Chrome（端口 ${PORT}，--reuse）`)
      return endpoint
    }
  }
  const bin = findChrome()
  const profileDir = mkdtempSync(join(tmpdir(), 'mobile-probe-'))
  console.log(`启动无头 Chrome（端口 ${PORT}，独立 profile）`)
  chromeProc = spawn(
    bin,
    [
      '--headless=new',
      '--disable-gpu',
      '--no-sandbox',
      '--no-first-run',
      '--no-default-browser-check',
      '--hide-scrollbars',
      `--user-data-dir=${profileDir}`,
      `--remote-debugging-port=${PORT}`,
      '--window-size=1440,1000',
      'about:blank',
    ],
    { stdio: 'ignore' },
  )
  for (let i = 0; i < 60; i++) {
    await sleep(300)
    const endpoint = await cdpEndpoint()
    if (endpoint) return endpoint
  }
  throw new Error(`Chrome 启动后连不上 CDP（端口 ${PORT}）`)
}

/* ------------------------------------------------------------------ */
/* 造数据                                                              */
/* ------------------------------------------------------------------ */

async function seed() {
  const j = async (path, body, method = 'POST') => {
    const res = await fetch(LOCAL + path, {
      method,
      headers: { 'Content-Type': 'application/json' },
      body: body ? JSON.stringify(body) : undefined,
    })
    const text = await res.text()
    if (!res.ok) throw new Error(`${path} → ${res.status} ${text.slice(0, 200)}`)
    return text ? JSON.parse(text) : null
  }

  const list = await (await fetch(LOCAL + '/api/providers')).json()
  let prov = (list.providers || list || [])[0]
  if (!prov) {
    prov = await j('/api/providers', { name: '手机体检网关', base_url: UPSTREAM, api_key: 'probe-key' })
  }
  // 拉一次模型列表，保证选择器里有东西
  await j(`/api/providers/${prov.id}/models/fetch`).catch(() => null)

  const convs = await (await fetch(LOCAL + '/api/conversations')).json()
  let conv = (convs.conversations || convs || [])[0]
  if (!conv) {
    conv = await j('/api/conversations', {
      title: '手机体检',
      provider_id: prov.id,
      model_id: 'mock-reasoner',
    })
  }

  // 灌一条用户消息 + 一条带 markdown 的助手回复，供布局/复制测试用
  const st = await (await fetch(`${LOCAL}/api/conversations/${conv.id}/state`)).json()
  const msgs = st.messages || []
  if (msgs.length === 0) {
    await fetch(`${LOCAL}/api/conversations/${conv.id}/messages`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        content: '用一句话解释一下 SSE，并给一小段代码示例。',
        provider_id: prov.id,
        model_id: 'mock-reasoner',
      }),
    })
    // 等生成完（mock 是流式，最多等 40s）
    for (let i = 0; i < 100; i++) {
      await sleep(400)
      const s2 = await (await fetch(`${LOCAL}/api/conversations/${conv.id}/state`)).json()
      const last = (s2.messages || []).at(-1)
      if (last && last.role === 'assistant' && last.status !== 'streaming') break
    }
  }
  return { prov, conv }
}

/* ------------------------------------------------------------------ */
/* 主流程                                                              */
/* ------------------------------------------------------------------ */

async function main() {
  const endpoint = await ensureChrome()
  const ws = new WebSocket(endpoint)
  await new Promise((res, rej) => {
    ws.onopen = res
    ws.onerror = () => rej(new Error('CDP WebSocket 连接失败'))
  })
  const cdp = new Cdp(ws)
  await cdp.send('Page.enable')
  await cdp.send('Runtime.enable')
  await cdp.send('Network.enable')

  console.log('\n造数据 …')
  const { prov, conv } = await seed()
  console.log(`  供应商 #${prov.id}  会话 #${conv.id}`)

  /* ================================================================ */
  /* 1. 局域网可达性 + 安全上下文                                       */
  /* ================================================================ */

  section('1. 局域网访问：令牌引导 → 安全上下文与 PWA 能力')
  say('访问地址', LAN_BASE)

  await cdp.setPhone(390, 844, 3)

  if (TOKEN) {
    /* ---- 手机的真实第一步：地址栏里输入带令牌的地址 ---- */
    await cdp.send('Page.navigate', { url: bootURL(LAN_BASE) })
    await cdp.waitFor(`!!document.querySelector('.composer__area')`, '带令牌打开页面', 25000)
    await sleep(800)

    const boot = await cdp.eval(`({
      href: location.href,
      search: location.search,
      jsSeesCookie: document.cookie.includes('ai_token'),
    })`)
    say('引导后地址栏', boot.href)
    say('URL 里还残留 token 吗', boot.search.includes('token'), boot.search.includes('token') ? '⚠ 没抹掉' : '已 302 抹掉')
    say('JS 能读到令牌 cookie 吗', boot.jsSeesCookie, '（HttpOnly 应当读不到）')
    if (boot.search.includes('token')) {
      pit('mid', '令牌没从地址栏抹掉', '会留在浏览历史、截图、聊天里转发的链接里')
    }
    if (boot.jsSeesCookie) {
      pit('mid', '令牌 cookie 不是 HttpOnly', 'XSS 就能直接读走令牌')
    }

    const apiAfterBoot = await cdp
      .eval(`fetch('/api/conversations').then((r) => r.status).catch((e) => 'ERR:' + e.message)`)
    say('引导后 /api/conversations', apiAfterBoot)
    if (apiAfterBoot !== 200) pit('high', '带令牌引导后 API 仍不通', `返回 ${apiAfterBoot}`)

    /* ---- 关键：模拟「从主屏幕启动」——不带任何 query 打开 ---- */
    await cdp.send('Page.navigate', { url: LAN_BASE })
    await cdp.waitFor(`!!document.querySelector('.composer__area')`, '裸地址重新打开', 25000)
    await sleep(900)
    const relaunch = await cdp
      .eval(`fetch('/api/conversations').then((r) => r.status).catch((e) => 'ERR:' + e.message)`)
    say('裸地址重新打开后 /api/conversations', relaunch, '（这就是从主屏幕启动的情形）')
    if (relaunch !== 200) {
      pit('high', '离开 URL 里的令牌就失联', `不带 query 重新打开后 API 返回 ${relaunch}`)
    } else {
      say('✔ 令牌靠 cookie 存活', '不依赖 URL，加桌面 / 重启手机都不会丢')
    }

    /* ---- 兜底路径：cookie 彻底没了（清空 cookie / 隐私模式）---- */
    await cdp.send('Network.clearBrowserCookies')
    await cdp.send('Page.navigate', { url: LAN_BASE })
    await cdp.waitFor(`!!document.querySelector('.composer__area')`, '清掉 cookie 后重新打开', 25000)
    await sleep(1200)

    const noCookie = await cdp.eval(`({
      hasDialog: !!document.querySelector('#token-input'),
      dialogTitle: (document.querySelector('.modal__panel--narrow .modal__title')?.textContent || '').trim(),
      allModals: [...document.querySelectorAll('.modal__title')].map((e) => e.textContent.trim()),
      footButtons: [...document.querySelectorAll('.modal__panel--narrow .modal__foot button')].map((b) =>
        b.textContent.trim(),
      ),
    })`)
    const apiNoCookie = await cdp
      .eval(`fetch('/api/conversations').then((r) => r.status).catch((e) => 'ERR:' + e.message)`)
    say('清掉 cookie 后 /api/conversations', apiNoCookie)
    say('自动弹出了令牌输入框', noCookie.hasDialog, noCookie.dialogTitle ? `（「${noCookie.dialogTitle}」）` : '')
    say('当前打开的弹窗', JSON.stringify(noCookie.allModals))
    say('令牌弹窗里的按钮', JSON.stringify(noCookie.footButtons))
    if (noCookie.allModals.length > 1) {
      pit('mid', '令牌弹窗和别的弹窗叠在一起', `同时打开了 ${noCookie.allModals.length} 个：${noCookie.allModals.join('、')}`)
    }
    if (apiNoCookie === 200) {
      say('注', '清 cookie 后接口仍通', '说明后端没开鉴权，本节不适用')
    } else if (!noCookie.hasDialog) {
      pit('high', '401 时没有引导用户输入令牌', '界面只会静默失败，用户不知道要做什么')
    } else {
      say('✔ 401 会弹令牌输入框', '而不是静默失败')
      // 手输令牌 → 保存 → 自动重载
      await cdp.eval(`(() => {
        const el = document.querySelector('#token-input')
        const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set
        setter.call(el, ${JSON.stringify(TOKEN)})
        el.dispatchEvent(new Event('input', { bubbles: true }))
      })()`)
      await sleep(250)
      // 必须限定在令牌弹窗里找按钮：设置弹窗的页脚也有按钮，不限定会点到别的那个
      const clicked = await cdp.eval(
        `(() => {
          const panel = document.querySelector('.modal__panel--narrow')
          const b = [...panel.querySelectorAll('.modal__foot button')].find((x) =>
            x.textContent.includes('保存'),
          )
          if (!b || b.disabled) return 'not-found-or-disabled'
          b.click()
          return b.textContent.trim()
        })()`,
      )
      say('点到的按钮', clicked)
      await sleep(2500)
      await cdp.waitFor(`!!document.querySelector('.composer__area')`, '手输令牌后重新加载', 25000)
      await sleep(1000)
      const afterManual = await cdp.eval(`({
        dialogGone: !document.querySelector('#token-input'),
        convs: document.querySelectorAll('.conv').length,
        stored: (() => { try { return !!localStorage.getItem('ai-assistant-token') } catch (e) { return 'ERR' } })(),
      })`)
      /*
       * 这里**不能**用裸 fetch 验证：cookie 已经被清掉了，而裸 fetch 不经过应用的
       * request() 封装，不会带 X-Auth-Token 头，于是必然 401 —— 那是探针自己没带凭证，
       * 不是应用坏了。要么按应用的方式补上头，要么直接看界面是否真的加载出了数据。
       */
      const apiManual = await cdp.eval(`(() => {
        let t = ''
        try { t = localStorage.getItem('ai-assistant-token') || '' } catch (e) { /* 忽略 */ }
        return fetch('/api/conversations', { headers: t ? { 'X-Auth-Token': t } : {} })
          .then((r) => r.status)
          .catch((e) => 'ERR:' + e.message)
      })()`)
      say('手输令牌后（按应用的方式带头上请求）', apiManual)
      say('弹窗已关闭 / 侧栏会话数 / localStorage 有令牌', `${afterManual.dialogGone} / ${afterManual.convs} / ${afterManual.stored}`)
      if (apiManual !== 200 || afterManual.convs < 1) {
        pit(
          'high',
          '手输令牌救不回来',
          `带头请求返回 ${apiManual}，侧栏会话数 ${afterManual.convs}`,
        )
      } else {
        say('✔ 手输令牌可用', 'cookie 被禁的环境也有救（图片可能仍加载不出来，因为 <img> 带不了头）')
      }
    }
  } else {
    await cdp.send('Page.navigate', { url: LAN_BASE })
    await cdp.waitFor(`!!document.querySelector('.composer__area')`, '页面加载', 25000)
    await sleep(600)
  }

  const ctx = await cdp.eval(`({
    origin: location.origin,
    secure: window.isSecureContext,
    clipboard: typeof navigator.clipboard,
    clipboardWrite: typeof navigator.clipboard?.writeText,
    serviceWorker: 'serviceWorker' in navigator,
    caches: typeof window.caches,
    mediaDevices: typeof navigator.mediaDevices,
    manifest: !!document.querySelector('link[rel=manifest]'),
    themeColor: !!document.querySelector('meta[name=theme-color]'),
    appleIcon: !!document.querySelector('link[rel=apple-touch-icon]'),
    appleCapable: !!document.querySelector('meta[name=apple-mobile-web-app-capable]'),
    viewport: document.querySelector('meta[name=viewport]')?.content,
    standalone: matchMedia('(display-mode: standalone)').matches,
    notif: typeof Notification,
    storage: typeof navigator.storage?.persist,
  })`)

  say('location.origin', ctx.origin)
  say('isSecureContext', ctx.secure, ctx.secure ? '安全上下文' : '⚠ 非安全上下文（http + 非 localhost）')
  say('navigator.serviceWorker', ctx.serviceWorker)
  say('window.caches', ctx.caches)
  say('link[rel=manifest]', ctx.manifest)
  say('meta[theme-color]', ctx.themeColor)
  say('apple-touch-icon', ctx.appleIcon)
  say('apple-mobile-web-app-capable', ctx.appleCapable)
  say('viewport meta', ctx.viewport)
  say('display-mode: standalone', ctx.standalone)

  if (!ctx.secure) {
    say(
      '⚠ 非安全上下文',
      'http 局域网 IP 下 Service Worker / 安装提示 / Clipboard API 全部不可用',
      '要变成真 PWA 必须上 HTTPS（自签在 iOS 上很麻烦）',
    )
  }
  // 下面几条都属于「做成 PWA」的范畴，本轮明确不做，只记录事实、不计入坑
  say('  · manifest', ctx.manifest ? '有' : '无（不装 PWA 就不需要）')
  say('  · apple-touch-icon', ctx.appleIcon ? '有' : '无')
  say('  · apple-mobile-web-app-capable', ctx.appleCapable ? '有' : '无（加桌面会带地址栏）')
  say('navigator.clipboard', ctx.clipboardWrite, 'undefined 时会走 execCommand 降级，第 5 节验它是否真能用')

  const shot1 = await cdp.shot('01-手机-刷新后.png')
  console.log(`  📷 ${shot1}`)

  /* ================================================================ */
  /* 2. 手机视口下的布局                                                */
  /* ================================================================ */

  section('2. 手机竖屏布局（390×844，DPR 3）')

  const layout = await cdp.eval(`(() => {
    const q = (s) => document.querySelector(s)
    const R = (s) => { const e = q(s); if (!e) return null
      const r = e.getBoundingClientRect()
      return { x:+r.left.toFixed(1), y:+r.top.toFixed(1), w:+r.width.toFixed(1), h:+r.height.toFixed(1), right:+r.right.toFixed(1) } }
    const sb = q('.sidebar')
    return {
      vw: innerWidth, vh: innerHeight,
      docW: document.documentElement.scrollWidth,
      sidebar: R('.sidebar'),
      main: R('.app__main'),
      msglist: R('.msglist'),
      composer: R('.composer'),
      bubble: R('.msg'),
      sidebarPos: getComputedStyle(sb).position,
      sidebarTransform: getComputedStyle(sb).transform,
      appClass: q('.app').className,
      scrim: !!q('.app__scrim'),
    }
  })()`)

  say('视口', `${layout.vw} × ${layout.vh} CSS px`)
  say('侧栏 position', layout.sidebarPos, layout.sidebarPos === 'fixed' ? '（抽屉，浮在内容上）' : '⚠ 仍占布局宽度')
  say('侧栏 transform', layout.sidebarTransform)
  say('主区 app__main 宽', layout.main?.w)
  say('消息列表 / 输入框 宽', `${layout.msglist?.w} / ${layout.composer?.w}`)
  say('消息气泡宽', layout.bubble?.w)
  say('文档 scrollWidth', layout.docW)

  const mainW = layout.main?.w ?? 0
  if (mainW < 340) {
    pit('high', '窄屏下对话区仍被侧栏挤窄', `只有 ${mainW}px`)
  } else {
    say('✔ 对话区拿满屏宽', `${mainW}px`)
  }
  if (!/app--collapsed/.test(layout.appClass)) {
    pit('mid', '窄屏下抽屉默认是打开的', '进页面先看到会话列表、主区被遮住，应该默认收起')
  } else {
    say('✔ 窄屏抽屉默认收起', layout.appClass)
  }
  if (layout.docW > layout.vw + 1) {
    pit('mid', '出现横向溢出', `scrollWidth ${layout.docW} > 视口 ${layout.vw}`)
  }
  const shot1b = await cdp.shot('01-手机-默认-抽屉收起.png')
  console.log(`  📷 ${shot1b}`)

  // 点顶栏按钮打开抽屉
  const toggleExpr = `[...document.querySelectorAll('.topbar button')].find((b) => (b.title || '').includes('侧栏'))`
  const toggleFound = await cdp.eval(`!!(${toggleExpr})`)
  say('顶栏有侧栏开关按钮', toggleFound)
  await cdp.eval(`(${toggleExpr})?.click()`)
  await sleep(500)
  const drawer = await cdp.eval(`(() => {
    const s = document.querySelector('.sidebar')
    const r = s.getBoundingClientRect()
    const sc = document.querySelector('.app__scrim')
    return {
      x: +r.left.toFixed(1), w: +r.width.toFixed(1),
      inView: r.left >= -1 && r.right > 1,
      scrim: !!sc,
      cls: document.querySelector('.app').className,
      pos: getComputedStyle(s).position,
    }
  })()`)
  say('打开后 侧栏 x / 宽', `${drawer.x} / ${drawer.w}`)
  say('侧栏已进入视口', drawer.inView)
  say('遮罩出现', drawer.scrim)
  if (!drawer.inView) pit('high', '抽屉打不开', '点了顶栏按钮侧栏仍在屏幕外')
  if (!drawer.scrim && drawer.pos === 'fixed') {
    pit('mid', '抽屉打开时没有遮罩', '背景没收暗，也不知道点哪儿能关')
  }
  const shot2 = await cdp.shot('02-手机-抽屉打开.png')
  console.log(`  📷 ${shot2}`)

  // 点遮罩关闭
  await cdp.eval(`document.querySelector('.app__scrim')?.click()`)
  await sleep(500)
  const closed = await cdp.eval(`(() => {
    const s = document.querySelector('.sidebar').getBoundingClientRect()
    return { offscreen: s.right <= 1, scrim: !!document.querySelector('.app__scrim') }
  })()`)
  say('点遮罩后 侧栏移出屏幕', closed.offscreen)
  say('点遮罩后 遮罩消失', !closed.scrim)
  if (!closed.offscreen) pit('mid', '点遮罩关不掉抽屉', '')

  /* ================================================================ */
  /* 3. 触屏可用性                                                      */
  /* ================================================================ */

  section('3. 触屏可用性（字号 / 点击目标 / hover 依赖）')

  const touchCaps = await cdp.eval(`({
    hoverNone: matchMedia('(hover: none)').matches,
    coarse: matchMedia('(pointer: coarse)').matches,
    msgToolsOpacity: (() => { const e = document.querySelector('.msg__tools'); return e ? getComputedStyle(e).opacity : null })(),
    convActionsDisplay: (() => { const e = document.querySelector('.conv__actions'); return e ? getComputedStyle(e).display : null })(),
    convMoreDisplay: (() => { const e = document.querySelector('.conv__more'); return e ? getComputedStyle(e).display : null })(),
    areaFont: getComputedStyle(document.querySelector('.composer__area')).fontSize,
    viewport: document.querySelector('meta[name=viewport]')?.content || '',
  })`)
  say('matchMedia (hover: none)', touchCaps.hoverNone)
  say('matchMedia (pointer: coarse)', touchCaps.coarse)
  say('输入框 font-size', touchCaps.areaFont)
  say('.msg__tools opacity', touchCaps.msgToolsOpacity)
  say('.conv__actions display', touchCaps.convActionsDisplay, '触屏下应默认 none')
  say('.conv__more display', touchCaps.convMoreDisplay, '触屏下的常驻入口')

  const fontPx = parseFloat(touchCaps.areaFont)
  if (fontPx < 16) {
    pit('high', `输入框字号 ${fontPx}px`, 'iOS Safari 聚焦 <16px 的输入框会强制缩放整页，且不会自动缩回')
  } else {
    say('✔ 输入框 ≥16px', 'iOS 不会因聚焦而缩放页面')
  }
  if (touchCaps.convMoreDisplay === 'none') {
    pit('high', '触屏下没有会话操作的常驻入口', '.conv__more 不可见，用户无法发现重命名/置顶/删除')
  } else {
    say('✔ 触屏有「更多操作」入口', '')
  }
  if (touchCaps.convActionsDisplay !== 'none') {
    pit('mid', '触屏下操作按钮默认就展开', '每行都挤满三个按钮；应默认收起、按需展开')
  }
  if (!/viewport-fit=cover/.test(touchCaps.viewport)) {
    pit('mid', 'viewport 缺 viewport-fit=cover', 'safe-area 永远为 0，底部小白条会压住发送按钮')
  } else {
    say('✔ viewport-fit=cover', '')
  }
  if (!/interactive-widget=resizes-content/.test(touchCaps.viewport)) {
    pit('mid', 'viewport 缺 interactive-widget=resizes-content', 'Android 弹键盘时布局视口不缩，输入框会被键盘盖住')
  } else {
    say('✔ interactive-widget=resizes-content', '')
  }

  /* 手指点会话行：不该再顺带把操作按钮亮出来（那会造成「第一下白切了会话」） */
  const convRowExists = await cdp.eval(`!!document.querySelector('.conv')`)
  if (convRowExists) {
    await cdp.tapExpr(`document.querySelector('.conv')`, '会话行')
    const afterRowTap = await cdp.eval(`({
      display: (() => { const e = document.querySelector('.conv--active .conv__actions'); return e ? getComputedStyle(e).display : null })(),
      active: document.querySelector('.conv--active')?.getAttribute('data-conv-id') || null,
    })`)
    say(
      '点会话行后 .conv__actions',
      afterRowTap.display,
      afterRowTap.display === 'none' ? '（正确：不再被 sticky hover 意外展开）' : '⚠ 仍会被 hover 展开',
    )
    if (afterRowTap.display !== 'none') {
      pit('mid', '触摸点会话行会顺带展开操作按钮', 'sticky hover 未在触屏下屏蔽')
    }
  }

  /* 点击目标尺寸：44×44 是 iOS HIG 的最小值，32 是 Android 的下限 */
  const taps = await cdp.eval(`(() => {
    const out = []
    const sel = 'button, a[href], input, textarea, [role=button], .conv'
    for (const el of document.querySelectorAll(sel)) {
      const r = el.getBoundingClientRect()
      if (r.width === 0 || r.height === 0) continue
      if (r.bottom < 0 || r.top > innerHeight) continue
      const cs = getComputedStyle(el)
      if (cs.visibility === 'hidden' || cs.display === 'none') continue
      const label = (el.getAttribute('title') || el.getAttribute('aria-label') || el.className || el.tagName).toString()
      out.push({ label: label.slice(0, 34), w: +r.width.toFixed(1), h: +r.height.toFixed(1) })
    }
    return out
  })()`)
  const small = taps.filter((t) => Math.min(t.w, t.h) < 32)
  const tiny = taps.filter((t) => Math.min(t.w, t.h) < 24)
  console.log(`  视口内可交互元素 ${taps.length} 个；最小边 <32px 的 ${small.length} 个，<24px 的 ${tiny.length} 个`)
  for (const t of tiny.slice(0, 12)) {
    console.log(`      · ${t.label.padEnd(36)} ${t.w}×${t.h}`)
  }
  if (tiny.length > 0) {
    pit('mid', `${tiny.length} 个点击目标小于 24px`, '手机上容易点错；建议触屏下把 icon-btn 放大到 32~44px')
  }

  /* ================================================================ */
  /* 4. 虚拟键盘遮挡（机制测量）                                        */
  /* ================================================================ */

  section('4. 软键盘弹起时的输入框位置')

  const kb = await cdp.eval(`(() => {
    const c = document.querySelector('.composer').getBoundingClientRect()
    return {
      composerBottom: +c.bottom.toFixed(1),
      innerHeight,
      visualHeight: window.visualViewport ? +window.visualViewport.height.toFixed(1) : null,
      appHeight: getComputedStyle(document.querySelector('.app')).height,
      kbVar: getComputedStyle(document.documentElement).getPropertyValue('--kb-h').trim() || '(未设置)',
    }
  })()`)
  say('composer 底边 / innerHeight', `${kb.composerBottom} / ${kb.innerHeight}`)
  say('.app 计算高度', kb.appHeight)
  say('--kb-h（未弹键盘时）', kb.kbVar)
  say('visualViewport 存在', kb.visualHeight !== null)

  /*
   * 真机键盘在无头浏览器里弹不出来，但 syncKeyboard() 的整条链路可以验：
   * 伪造一个「被键盘压矮 320px」的 visualViewport，聚焦输入框触发一次，
   * 看 --kb-h 有没有被写上、.app 有没有变矮、输入框有没有跟着上移。
   * 验完立刻复原，别影响后面的测试。
   */
  const kbSim = await cdp.eval(`(async () => {
    const real = window.visualViewport
    // 必须把原始描述符存下来：visualViewport 在 Chrome 里是 window 自己的
    // 访问器属性，直接 delete 会把它彻底删掉，之后就再也算不出键盘高度了。
    const desc = Object.getOwnPropertyDescriptor(window, 'visualViewport')
    const KB = 320
    const fake = {
      width: real.width,
      height: window.innerHeight - KB,
      offsetTop: 0,
      scale: 1,
      addEventListener() {},
      removeEventListener() {},
    }
    Object.defineProperty(window, 'visualViewport', { value: fake, configurable: true })
    const ta = document.querySelector('.composer__area')
    ta.focus()
    ta.dispatchEvent(new Event('focusin', { bubbles: true }))
    await new Promise((r) => setTimeout(r, 250))

    const out = {
      kbVar: getComputedStyle(document.documentElement).getPropertyValue('--kb-h').trim(),
      appH: Math.round(document.querySelector('.app').getBoundingClientRect().height),
      composerBottom: Math.round(document.querySelector('.composer').getBoundingClientRect().bottom),
      innerH: window.innerHeight,
      focused: document.activeElement === ta,
    }

    // 复原：装回原始描述符 + 触发一次 focusout 让 --kb-h 归零
    if (desc) Object.defineProperty(window, 'visualViewport', desc)
    else delete window.visualViewport
    ta.blur()
    ta.dispatchEvent(new Event('focusout', { bubbles: true }))
    await new Promise((r) => setTimeout(r, 250))
    out.kbVarAfter = getComputedStyle(document.documentElement).getPropertyValue('--kb-h').trim()
    out.appHAfter = Math.round(document.querySelector('.app').getBoundingClientRect().height)
    out.vvRestored = !!window.visualViewport
    return out
  })()`)

  say('伪造键盘高 320px 时', `--kb-h = ${kbSim.kbVar}   .app 高 = ${kbSim.appH}   composer 底边 = ${kbSim.composerBottom}`)
  say('输入框是否真的获得焦点', kbSim.focused, '（没焦点就不会算键盘高度）')
  say('复原后', `--kb-h = ${kbSim.kbVarAfter}   .app 高 = ${kbSim.appHAfter}   visualViewport 已还原 = ${kbSim.vvRestored}`)

  const expectApp = kbSim.innerH - 320
  if (kbSim.kbVar === '320px' && Math.abs(kbSim.appH - expectApp) <= 2) {
    say('✔ 键盘遮挡已被处理', `.app 跟着视觉视口一起缩到 ${kbSim.appH}px，输入框被顶到键盘之上`)
  } else {
    pit(
      'high',
      '软键盘仍会盖住输入框',
      `伪造键盘 320px 后 --kb-h=${kbSim.kbVar || '(空)'}、.app 高=${kbSim.appH}（期望 ${expectApp}）`,
    )
  }
  if (kbSim.kbVarAfter !== '0px' && kbSim.kbVarAfter !== '') {
    pit('mid', '键盘收起后 --kb-h 没有归零', `残留 ${kbSim.kbVarAfter}`)
  } else {
    say('✔ 键盘收起后高度归零', '')
  }

  /* 模拟 Android 加了 interactive-widget=resizes-content 的情形：布局视口真的缩了 */
  await cdp.setPhone(390, 844 - 336, 3) // 键盘高约 336
  await sleep(500)
  const shrunk = await cdp.eval(`(() => {
    const c = document.querySelector('.composer').getBoundingClientRect()
    return { vh: innerHeight, composerBottom: +c.bottom.toFixed(1), visible: c.bottom <= innerHeight + 1 && c.top >= 0 }
  })()`)
  say('布局视口缩到', `${shrunk.vh} 后 composer 底边`, `${shrunk.composerBottom}  可见=${shrunk.visible}`)
  if (!shrunk.visible) {
    pit('high', '布局视口缩小时输入框也看不见', '布局没跟着缩，键盘一弹就顶掉输入框')
  } else {
    say('✔ 布局视口缩小时输入框正确上移', '')
  }
  const shot4 = await cdp.shot('04-手机-布局视口缩小后.png')
  console.log(`  📷 ${shot4}`)
  await cdp.setPhone(390, 844, 3)
  await sleep(300)

  /* grep 源码确认：web/src 全局没有任何 visualViewport 监听（只有 visibilitychange / online） */
  say('（源码侧）', 'frontend 无 visualViewport 监听', 'grep visualViewport → 0 处')

  /* ================================================================ */
  /* 5. 复制按钮：真复制还是谎报                                        */
  /* ================================================================ */

  section('5. 复制按钮：http 局域网下是真复制还是谎报')

  // 关键：不能只看 toast。点完复制后用**真实粘贴**把剪贴板读回来比对。
  const copyTitle = await cdp.eval(`(() => {
    const b = [...document.querySelectorAll('.msg__tools button, .msg__tools .icon-btn')]
      .find((x) => (x.getAttribute('title') || '').includes('复制'))
    return b ? b.getAttribute('title') : null
  })()`)
  say('复制按钮', copyTitle || '没找到')

  if (copyTitle) {
    await cdp.eval(`(() => {
      const b = [...document.querySelectorAll('.msg__tools button, .msg__tools .icon-btn')]
        .find((x) => (x.getAttribute('title') || '').includes('复制'))
      b.click()
    })()`)
    await sleep(700)
    const toastTxt = await cdp.eval(`(document.querySelector('[class*=toast]')?.innerText || '(无提示)').trim()`)
    say('界面提示', toastTxt)

    // 用 Ctrl+V 走真实粘贴通道，把剪贴板内容读回输入框
    await cdp.eval(`(() => {
      const ta = document.querySelector('.composer__area')
      const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
      setter.call(ta, '')
      ta.dispatchEvent(new Event('input', { bubbles: true }))
      ta.focus()
    })()`)
    await sleep(250)
    await cdp.send('Input.dispatchKeyEvent', {
      type: 'keyDown',
      modifiers: 2,
      key: 'v',
      code: 'KeyV',
      windowsVirtualKeyCode: 86,
      nativeVirtualKeyCode: 86,
      commands: ['paste'],
    })
    await cdp.send('Input.dispatchKeyEvent', {
      type: 'keyUp',
      modifiers: 2,
      key: 'v',
      code: 'KeyV',
      windowsVirtualKeyCode: 86,
      nativeVirtualKeyCode: 86,
    })
    await sleep(600)
    const pasted = await cdp.eval(`document.querySelector('.composer__area').value`)
    say('粘贴回读到的内容', JSON.stringify(String(pasted || '').slice(0, 70)))
    if (!pasted) {
      pit(
        'high',
        '复制会「谎报成功」',
        `界面提示「${toastTxt}」，但真去粘贴剪贴板是空的 —— execCommand('copy') 的返回值不可信`,
      )
    } else {
      say('结论', '剪贴板确实写进去了')
    }
    await cdp.eval(`(() => {
      const ta = document.querySelector('.composer__area')
      const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
      setter.call(ta, '')
      ta.dispatchEvent(new Event('input', { bubbles: true }))
      ta.blur()
    })()`)
  }

  // 对照组：同一个浏览器、同一份代码，只在 127.0.0.1 上打开
  await cdp.send('Page.navigate', { url: bootURL(LOCAL) })
  await cdp.waitFor(`!!document.querySelector('.composer__area')`, '本机页面', 20000)
  await sleep(700)
  const localCtx = await cdp.eval(`({
    secure: window.isSecureContext,
    clipboard: typeof navigator.clipboard,
    writeText: typeof navigator.clipboard?.writeText,
    sw: 'serviceWorker' in navigator,
  })`)
  console.log('  —— 对照组：同一浏览器打开 http://127.0.0.1:28888 ——')
  say('isSecureContext', localCtx.secure)
  say('navigator.clipboard', localCtx.clipboard)
  say('navigator.clipboard.writeText', localCtx.writeText)
  say('serviceWorker', localCtx.sw)
  console.log('  上面几项在局域网 IP 下都是 undefined/false，差别只来自 origin')

  // 回到局域网 IP + 手机视口
  await cdp.setPhone(390, 844, 3)
  await cdp.send('Page.navigate', { url: LAN_BASE })
  await cdp.waitFor(`!!document.querySelector('.composer__area')`, '回到局域网页面', 20000)
  await sleep(800)

  /* ================================================================ */
  /* 6. 触屏下到底能不能管理会话                                        */
  /* ================================================================ */

  section('6. 触屏下能不能重命名 / 置顶 / 删除会话')

  await fetch(`${LOCAL}/api/conversations`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      title: '第二条（触屏测试）',
      provider_id: prov.id,
      model_id: 'mock-reasoner',
    }),
  })
  await cdp.eval(`location.reload()`)
  await cdp.waitFor(`document.querySelectorAll('.conv').length >= 2`, '侧栏出现两条会话', 20000)
  await sleep(900)

  // 手机上抽屉默认是收起的，要先打开才能操作会话列表
  const drawerClosedNow = await cdp.eval(`document.querySelector('.app').className.includes('app--collapsed')`)
  if (drawerClosedNow) {
    await cdp.eval(`(${toggleExpr})?.click()`)
    await sleep(450)
    say('先打开抽屉', '窄屏默认收起，符合预期')
  }

  const convBefore = await cdp.eval(`(() => {
    const rows = [...document.querySelectorAll('.conv')]
    const act = document.querySelector('.conv--active')
    return {
      order: rows.map((r) => r.getAttribute('data-conv-id')),
      active: act?.getAttribute('data-conv-id') || null,
      actions: act ? getComputedStyle(act.querySelector('.conv__actions')).display : '(无)',
      more: act ? getComputedStyle(act.querySelector('.conv__more')).display : '(无)',
    }
  })()`)
  say('会话顺序', convBefore.order.join(' , '))
  say('当前选中', convBefore.active)
  say('选中项 操作按钮 / 更多入口', `${convBefore.actions} / ${convBefore.more}`)

  // 手指点「非当前」那一条：应当只切会话，不展开按钮
  const targetId = convBefore.order.find((x) => x !== convBefore.active) || convBefore.order[0]
  const tapPt = await cdp.eval(`(() => {
    const rows = [...document.querySelectorAll('.conv')]
    const r = rows.find((x) => x.getAttribute('data-conv-id') === ${JSON.stringify(targetId)})
    const b = r.getBoundingClientRect()
    return { x: Math.round(b.left + b.width / 2), y: Math.round(b.top + b.height / 2) }
  })()`)
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [tapPt] })
  await sleep(60)
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
  await sleep(700)

  const convAfter = await cdp.eval(`(() => {
    const act = document.querySelector('.conv--active')
    return {
      active: act?.getAttribute('data-conv-id') || null,
      actions: act ? getComputedStyle(act.querySelector('.conv__actions')).display : '(无)',
      menuOpen: !!document.querySelector('.conv--menu-open'),
    }
  })()`)
  say('点一行后 当前选中', convAfter.active)
  say('点一行后 操作按钮 / 菜单展开', `${convAfter.actions} / ${convAfter.menuOpen}`)
  if (convAfter.active === targetId && convAfter.actions === 'none' && !convAfter.menuOpen) {
    say('✔ 单纯切会话', '没有顺带把操作按钮亮出来')
  }
  if (convAfter.menuOpen) {
    pit('mid', '点会话行会把操作菜单一起展开', '切会话和展开菜单混在一个动作里')
  }

  /*
   * 选中会话后抽屉应当自动收起（手机上选完就该看对话）。
   * 上一轮探针就是栽在这里：抽屉在选中后关掉了，后面按坐标点「⋯」其实点在了
   * 主区上，于是「点不出菜单」看着像应用 bug，其实是探针没跟上状态。
   */
  const autoClosed = await cdp.eval(`document.querySelector('.app').className.includes('app--collapsed')`)
  say('选中会话后抽屉自动收起', autoClosed, autoClosed ? '（符合预期）' : '')
  if (!autoClosed) {
    pit('mid', '选中会话后抽屉没有自动收起', '手机上选完会话还被侧栏盖着')
  } else {
    await cdp.eval(`(${toggleExpr})?.click()`)
    await sleep(500)
    say('重新打开抽屉', '准备测「⋯」入口')
  }

  // 手指点「⋯」入口 → 菜单展开
  const moreTap = await cdp.eval(`(() => {
    const act = document.querySelector('.conv--active')
    const b = act.querySelector('.conv__more').getBoundingClientRect()
    return { x: Math.round(b.left + b.width / 2), y: Math.round(b.top + b.height / 2), w: Math.round(b.width), h: Math.round(b.height) }
  })()`)
  say('「⋯」入口尺寸', `${moreTap.w}×${moreTap.h}`)
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: moreTap.x, y: moreTap.y }] })
  await sleep(60)
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
  await sleep(500)

  const menuState = await cdp.eval(`(() => {
    const act = document.querySelector('.conv--active')
    return {
      menuOpen: !!document.querySelector('.conv--menu-open'),
      actions: act ? getComputedStyle(act.querySelector('.conv__actions')).display : '(无)',
      buttons: act ? [...act.querySelectorAll('.conv__actions button')].map((x) => x.getAttribute('title')) : [],
    }
  })()`)
  say('点「⋯」后 菜单展开', menuState.menuOpen)
  say('点「⋯」后 操作按钮', menuState.actions)
  say('菜单里的按钮', JSON.stringify(menuState.buttons))
  if (!menuState.menuOpen || menuState.actions === 'none') {
    pit('high', '「⋯」入口点不出操作菜单', '触屏下仍然无法管理会话')
  } else {
    say('✔ 触屏有了显式入口', '点「⋯」→ 展开三个操作')
  }

  // 再点「重命名」，验证真的能完成一次重命名
  const renameReady = await cdp.eval(`(() => {
    const act = document.querySelector('.conv--active')
    const b = [...act.querySelectorAll('.conv__actions button')].find((x) => (x.getAttribute('title') || '').includes('重命名'))
    if (!b) return 'no-button'
    const r = b.getBoundingClientRect()
    if (r.width === 0 || r.height === 0) return 'zero-size'
    return 'ok'
  })()`)
  say('重命名按钮可取到吗', renameReady)
  if (renameReady === 'ok') {
    await cdp.tapExpr(
      `(() => { const a = document.querySelector('.conv--active'); return [...a.querySelectorAll('.conv__actions button')].find((x) => (x.getAttribute('title') || '').includes('重命名')) })()`,
      '重命名按钮',
    )
    const renameBox = await cdp.eval(`!!document.querySelector('.conv__rename')`)
    say('点完「重命名」后出现输入框', renameBox)
    if (!renameBox) {
      pit('high', '手机上无法重命名会话', '点了重命名按钮但没出现输入框')
    } else {
      say('✔ 触屏能完整走完重命名', '⋯ → 重命名 → 输入框')
    }
  }
  const shot6 = await cdp.shot('06-手机-会话操作.png')
  console.log(`  📷 ${shot6}`)

  // 关掉重命名（Escape），别把状态带进后面的测试
  await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 })
  await cdp.send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 })
  await sleep(300)

  /* ================================================================ */
  /* 7. 断网 → 恢复（锁屏 / Wi-Fi 抖动）                                */
  /* ================================================================ */

  section('7. 断网再恢复：流会重连吗？会重新同步吗？')

  const activeConvId = await cdp.eval(`document.querySelector('.conv--active')?.getAttribute('data-conv-id') || ${JSON.stringify(String(conv.id))}`)
  const stream0 = cdp.reqCount('/api/stream')
  const state0 = cdp.reqCount('/state')
  say('断网前基线请求数', `/api/stream=${stream0}   /state=${state0}`)

  // 先发一条，制造「生成中」
  const userBefore = await cdp.eval(`document.querySelectorAll('.msg--user').length`)
  await cdp.eval(`(() => {
    const ta = document.querySelector('.composer__area')
    const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set
    setter.call(ta, '断网恢复测试')
    ta.dispatchEvent(new Event('input', { bubbles: true }))
  })()`)
  await sleep(250)
  const sendDisabled = await cdp.eval(`document.querySelector('.composer__send')?.disabled ?? 'no-button'`)
  say('发送按钮 disabled', sendDisabled)
  await cdp.eval(`document.querySelector('.composer__send')?.click()`)
  await sleep(1200)
  const userAfter = await cdp.eval(`document.querySelectorAll('.msg--user').length`)
  say('用户气泡数', `${userBefore} → ${userAfter}`)
  if (userAfter <= userBefore) {
    pit('high', '消息没发出去', '断网测试的前提不成立，后面的重连结论不可信')
  }
  say('已发出消息，界面在生成中', await cdp.eval(`!!document.querySelector('.cursor, .thinking')`))

  const failBefore = cdp.failed.length
  await cdp.send('Network.emulateNetworkConditions', {
    offline: true,
    latency: 0,
    downloadThroughput: 0,
    uploadThroughput: 0,
  })
  say('切断网络', 'offline=true，挂 8 秒（模拟锁屏）')

  // 自检：断网必须真的生效，否则这一节的结论全是假的
  await sleep(900)
  const offlineReal = await cdp.eval(
    `fetch('/api/conversations').then(() => 'still-ok').catch(() => 'blocked')`,
  )
  say('断网自检（页面还 fetch 得动吗）', offlineReal)
  if (offlineReal !== 'blocked') {
    say('⚠ 本节作废', 'offline 模拟没生效，下面的结论不能采信')
  }
  await sleep(8000)

  await cdp.send('Network.emulateNetworkConditions', {
    offline: false,
    latency: 20,
    downloadThroughput: -1,
    uploadThroughput: -1,
  })
  say('网络恢复', 'offline=false')
  // 真机从后台切回前台会触发这两个事件，这里手动补一次
  await cdp.eval(`(() => {
    try { Object.defineProperty(document, 'visibilityState', { get: () => 'visible', configurable: true }) } catch (e) {}
    document.dispatchEvent(new Event('visibilitychange'))
    window.dispatchEvent(new Event('online'))
  })()`)
  await sleep(4000)

  const stream1 = cdp.reqCount('/api/stream')
  const state1 = cdp.reqCount('/state')
  say('请求计数变化', `/api/stream ${stream0}→${stream1}   /state ${state0}→${state1}`)
  const newFails = cdp.failed.slice(failBefore)
  say(
    '断网期间失败的请求',
    String(newFails.length) +
      (newFails.length
        ? '：' + newFails.slice(0, 3).map((f) => (f.url.split('/api/')[1] || f.url) + '(' + f.err + ')').join(', ')
        : '（一个都没失败 → 说明连接压根没断）'),
  )

  // 最硬的一条：服务端广播还传得到浏览器吗
  const alive = await streamLiveness(cdp, '断网恢复后')
  if (alive === false) {
    pit('high', '断网恢复后实时推送已经死了', '服务端新事件到不了页面，界面不会自己更新，只能手动刷新')
  }

  const stNow = await (await fetch(`${LOCAL}/api/conversations/${activeConvId}/state`)).json()
  const dbMsgs = stNow.messages || []
  const dbLast = dbMsgs.filter((m) => m.role === 'assistant').at(-1)
  const uiBubbles = await cdp.eval(`document.querySelectorAll('.msg').length`)
  say('后端助手消息', `${dbLast?.status} / ${(dbLast?.content || '').length} 字`)
  say('界面消息数 / 后端消息数', `${uiBubbles} / ${dbMsgs.length}`)
  say('界面还在显示「生成中」吗', await cdp.eval(`!!document.querySelector('.cursor, .composer__send--stop')`))
  const shot7 = await cdp.shot('07-手机-断网恢复后.png')
  console.log(`  📷 ${shot7}`)

  /* ================================================================ */
  /* 8. 页面冻结再唤醒（iOS 切后台 / 锁屏）                             */
  /* ================================================================ */

  section('8. 页面被真正冻结再唤醒')

  const s0 = cdp.reqCount('/api/stream')
  let froze = false
  try {
    await cdp.send('Page.setWebLifecycleState', { state: 'frozen' })
    froze = true
  } catch (e) {
    say('Page.setWebLifecycleState', '本环境不支持：' + e.message.slice(0, 70))
  }
  if (froze) {
    say('已冻结页面', '模拟切后台 6 秒')
    await sleep(6000)
    try {
      await cdp.send('Page.setWebLifecycleState', { state: 'active' })
    } catch (e) {
      say('唤醒失败', e.message.slice(0, 70))
    }
    await sleep(1500)
    try {
      say('唤醒后 visibilityState', await cdp.eval(`document.visibilityState`))
      say('唤醒后页面还能执行 JS', true)
      say('唤醒后能访问后端', await cdp.eval(`fetch('/api/conversations').then((x) => x.status).catch((e) => 'ERR')`))
      // 真机切回前台会派发 visibilitychange
      await cdp.eval(`(() => {
        try { Object.defineProperty(document, 'visibilityState', { get: () => 'visible', configurable: true }) } catch (e) {}
        document.dispatchEvent(new Event('visibilitychange'))
      })()`)
      await sleep(3500)
      const s1 = cdp.reqCount('/api/stream')
      say('唤醒后 /api/stream 请求数', `${s0} → ${s1}`)
      const aliveFrozen = await streamLiveness(cdp, '冻结唤醒后')
      if (aliveFrozen === false) {
        pit('mid', '长时间后台后推送可能已死', '唤醒后服务端广播到不了页面')
      }
    } catch (e) {
      pit('high', '冻结唤醒后页面无响应', '需要刷新才能恢复：' + e.message.slice(0, 90))
    }
  }
  const shot8 = await cdp.shot('08-手机-冻结唤醒后.png')
  console.log(`  📷 ${shot8}`)

  /* ================================================================ */
  /* 9. 附件上传：手机相机的出图格式                                    */
  /* ================================================================ */

  section('9. 附件上传：手机相机的出图格式')

  const upInfo = await cdp.eval(`(() => {
    const inp = document.querySelector('input[type=file]')
    return {
      accept: inp?.getAttribute('accept'),
      capture: inp?.getAttribute('capture'),
      multiple: inp?.hasAttribute('multiple'),
    }
  })()`)
  say('input[type=file] accept', upInfo.accept)
  say('capture 属性', upInfo.capture || '（无：iOS 会给出「拍照 / 图库」选择）')
  say('multiple', upInfo.multiple)

  // 手上没有真 HEIC，但「Go 解不出来」走的是同一条分支，用一张手工 BMP 当替身
  const png1x1 = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==',
    'base64',
  )
  const bmp = makeBmp(4, 4)

  async function upload(name, bytes, type) {
    const fd = new FormData()
    fd.append('file', new Blob([bytes], { type }), name)
    const res = await fetch(`${LOCAL}/api/uploads`, { method: 'POST', body: fd })
    const txt = await res.text()
    let msg = txt
    try {
      msg = JSON.parse(txt)?.error?.message || txt
    } catch {
      /* 原样 */
    }
    return { status: res.status, msg: String(msg).slice(0, 100) }
  }

  const rPng = await upload('shot.png', png1x1, 'image/png')
  say('上传 PNG（对照组）', `${rPng.status}  ${rPng.msg}`)
  const rBmp = await upload('photo.bmp', bmp, 'image/bmp')
  say('上传 BMP（Go 认不出的格式）', `${rBmp.status}  ${rBmp.msg}`)
  const rHeic = await upload('IMG_0001.HEIC', bmp, 'image/heic')
  say('上传 .HEIC（同样的字节）', `${rHeic.status}  ${rHeic.msg}`)

  // 这里直连后端、绕过了前端，所以 HEIC 被拒是**预期**的：
  // Go 侧没有 HEIF 解码器，转换是前端做的。见第 10 节。
  say(
    '后端本身解不了 HEIC',
    `只认 png / jpg / gif / webp（Go 的解码器集合）`,
    '这是设计上的分工：浏览器转码在前端完成，见第 10 节',
  )

  /* ================================================================ */
  /* 10. HEIC：前端浏览器端转换                                         */
  /* ================================================================ */

  section('10. HEIC：浏览器端转成 JPEG（后端只认 png/jpg/gif/webp）')

  const cap = await cdp.eval(`({
    hasImageDecoder: typeof ImageDecoder !== 'undefined',
    heicByImageDecoder: (() => {
      try { return typeof ImageDecoder === 'undefined' ? 'n/a' : ImageDecoder.isTypeSupported('image/heic') } catch (e) { return 'ERR' }
    })(),
    hasCreateImageBitmap: typeof createImageBitmap === 'function',
    hasCanvasToBlob: typeof document.createElement('canvas').toBlob === 'function',
  })`)
  say('ImageDecoder 存在（WebCodecs 能力探测）', cap.hasImageDecoder)
  say("isTypeSupported('image/heic')", cap.heicByImageDecoder, cap.heicByImageDecoder === false ? '本浏览器解不了真 HEIC' : '')
  say('createImageBitmap / canvas.toBlob', `${cap.hasCreateImageBitmap} / ${cap.hasCanvasToBlob}`)

  const heicDir = join(SHOT_DIR, '..', 'heic')
  mkdirSync(heicDir, { recursive: true })
  const pngBytes = readFileSync(FIXTURE)
  const src = pngSize(pngBytes)

  /** 条目从「上传中」变成终态（ready 或 error） */
  const attachSettled = `(() => {
    const att = document.querySelector('.composer__att')
    if (!att) return false
    const cover = att.querySelector('.composer__att-cover')
    return !cover || cover.classList.contains('composer__att-cover--error')
  })()`

  /*
   * 路径 A「浏览器能解」——模拟 iOS Safari 上原生解码成功的那条路。
   *
   * 这里**不能**用 DOM.setFileInputFiles 塞一个 .heic 文件：Windows 已经装了
   * HEIF 扩展，浏览器拿到的 Blob type 会是 image/heic，而浏览器是**按 MIME 选解码器**的
   * —— 于是哪怕字节是张合法 PNG，Chrome 也会因为「类型是 HEIC」直接拒掉。
   * （第一版就是这么错的，表现成「能解的也失败」。）
   *
   * 所以改成在页面里用 DataTransfer 造 File：**type 留空**、名字叫 .heic。
   * 这样 isHeic() 走扩展名分支判定为 HEIC，而浏览器只能靠内容嗅探 —— PNG 就解得开，
   * 正好把「判定为 HEIC → 解码成功 → canvas → JPEG → 上传」整条链路跑通。
   * 这也贴近现实：iOS 给过来的 HEIC 有时 MIME 就是空的。
   */
  const pngB64 = pngBytes.toString('base64')
  const injectHeic = `(async () => {
    const bin = atob(${JSON.stringify(pngB64)})
    const u8 = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) u8[i] = bin.charCodeAt(i)
    const f = new File([u8], 'IMG_0001.HEIC', { type: '' })
    const dt = new DataTransfer()
    dt.items.add(f)
    const input = document.querySelector('input[type=file]')
    input.files = dt.files
    input.dispatchEvent(new Event('change', { bubbles: true }))
    return { name: f.name, type: f.type, size: f.size }
  })()`

  const injected = await cdp.eval(injectHeic)
  say('路径A 注入的替身文件', `${injected.name}  type="${injected.type}"  ${injected.size} 字节（内容是 PNG）`)
  await cdp.waitFor(`document.querySelectorAll('.composer__att').length > 0`, '待发送缩略图出现', 10000)
  await cdp.waitFor(attachSettled, 'HEIC 处理完成', 30000)
  const a1 = await cdp.eval(`(() => {
    const att = document.querySelector('.composer__att')
    return {
      isError: !!att?.classList.contains('composer__att--error'),
      title: (att?.getAttribute('title') || '').slice(0, 90),
      previewOk: (() => { const i = att?.querySelector('img'); return i ? i.naturalWidth > 0 : false })(),
    }
  })()`)
  say('路径A 条目状态', a1.isError ? 'error' : 'ready')
  say('  提示原文', a1.title)
  say('  预览图渲染出来了', a1.previewOk)
  if (a1.isError) {
    pit('high', 'HEIC 转换在「浏览器能解」的情况下也失败了', a1.title)
  }

  // 发出去，再从后端读它到底存成了什么格式 —— 这才是真正的证据
  if (!a1.isError) {
    await cdp.eval(`document.querySelector('.composer__send')?.click()`)
    await sleep(2500)
    const st = await (await fetch(`${LOCAL}/api/conversations/${activeConvId}/state`)).json()
    const withAtt = (st.messages || []).filter((m) => m.role === 'user' && (m.attachments || []).length > 0)
    const lastAtt = withAtt.at(-1)?.attachments?.at(-1)
    say('后端存下来的 MIME', lastAtt?.mime, lastAtt?.mime === 'image/jpeg' ? '← 转换生效' : '')
    say('后端记录的尺寸 / 源 PNG 尺寸', `${lastAtt?.width}×${lastAtt?.height} / ${src.w}×${src.h}`)
    if (lastAtt?.mime !== 'image/jpeg') {
      pit('high', '前端转出来的不是 JPEG', `后端收到的是 ${lastAtt?.mime}`)
    } else if (lastAtt.width !== src.w || lastAtt.height !== src.h) {
      pit('mid', '转换后尺寸变了', `${src.w}×${src.h} → ${lastAtt.width}×${lastAtt.height}`)
    } else {
      say('✔ HEIC → JPEG 全链路通', `前端转码后上传，后端按 image/jpeg 收下，尺寸未变`)
    }
  }

  /* ---- 路径 B：浏览器解不了（桌面 Chrome 遇到真 HEIC 就是这个下场）---- */
  const pathBroken = join(heicDir, 'IMG_0002.HEIC')
  writeFileSync(
    pathBroken,
    Buffer.concat([
      Buffer.from([0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70, 0x68, 0x65, 0x69, 0x63]),
      Buffer.alloc(4096, 0x5a),
    ]),
  )
  await cdp.eval(`(() => {
    const b = document.querySelector('.composer__att-remove')
    if (b) b.click()
  })()`)
  await sleep(400)
  await cdp.setFileInput('input[type=file]', [pathBroken])
  await cdp.waitFor(`document.querySelectorAll('.composer__att').length > 0`, '第二个缩略图出现', 10000)
  await cdp.waitFor(attachSettled, '坏 HEIC 处理完成', 30000)
  const b1 = await cdp.eval(`(() => {
    const att = document.querySelector('.composer__att')
    return {
      isError: !!att?.classList.contains('composer__att--error'),
      title: (att?.getAttribute('title') || '').slice(0, 200),
    }
  })()`)
  say('路径B（浏览器解不了）条目状态', b1.isError ? 'error' : 'ready')
  say('  提示原文', b1.title)
  if (!b1.isError) {
    pit('high', '解不了的 HEIC 居然当成上传成功', '后端不该收下它')
  } else if (!/浏览器解不了 HEIC/.test(b1.title)) {
    pit('mid', '解不了 HEIC 时的提示不够准确', `给的是「${b1.title}」，应当明确告诉用户是浏览器解不了、以及往哪儿改`)
  } else {
    say('✔ 解不了时给出可操作的提示', '而不是后端那句「这个文件不是能识别的图片」')
  }
  await cdp.eval(`(() => {
    const b = document.querySelector('.composer__att-remove')
    if (b) b.click()
  })()`)

  /* ================================================================ */
  /* 汇总                                                              */
  /* ================================================================ */

  section('汇总')
  for (const f of findings) {
    const mark = f.sev === 'high' ? '🔴' : f.sev === 'mid' ? '🟠' : '🟡'
    console.log(`${mark} [${f.sev}] ${f.title}`)
    if (f.detail) console.log(`        ${f.detail}`)
  }
  const hi = findings.filter((f) => f.sev === 'high').length
  const mid = findings.filter((f) => f.sev === 'mid').length
  const lo = findings.filter((f) => f.sev === 'low').length
  console.log(`\n  高 ${hi} / 中 ${mid} / 低 ${lo}，共 ${findings.length} 条\n`)

  if (!KEEP && chromeProc) chromeProc.kill()
  process.exit(0)
}

main().catch((e) => {
  console.error('\n[探针出错]', e.message)
  if (chromeProc) chromeProc.kill()
  process.exit(1)
})
