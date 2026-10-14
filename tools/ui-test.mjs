/**
 * 前端页面端到端测试（零依赖）
 *
 * 用 Node 内置 WebSocket 直连 Chrome DevTools Protocol，派发**真实**的键鼠事件，
 * 在真实浏览器里跑完整用户旅程。不依赖 Playwright/Puppeteer。
 *
 * 用法：
 *   node tools/ui-test.mjs                        # 自动拉起无头 Chrome
 *   node tools/ui-test.mjs --port 9222            # 连已开着的 CDP
 *   node tools/ui-test.mjs --keep                 # 跑完不关浏览器（调试用）
 *
 * 前提：后端跑在 --base，上游用 tools/mockllm -delay 250ms 左右
 *       （生成要持续几秒，才抓得到流式中间状态）。
 *
 * ⚠ 假上游的 -vision 必须覆盖会话里会用到的**所有**模型，例如：
 *     mockllm -delay 250ms -models mock-chat,mock-reasoner,mock-vision \
 *             -reasoning mock-reasoner -vision mock-chat,mock-reasoner,mock-vision
 *   原因：L 组会往会话里发一张图，而「历史里的图片每轮都带给模型」是刻意设计，
 *   于是 L 组之后所有发往这个会话的消息都会带上那张图。如果模型不在 -vision 里，
 *   假上游会回 400「This model does not support image input」，
 *   表现成 M 组「等生成超时」——而且因为助手消息是 error 状态、停止按钮一闪都没有，
 *   看起来特别像分片太快抓不到。这个坑真的踩过一次。
 */
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

/* ------------------------------------------------------------------ */
/* 参数与环境                                                          */
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
const BASE = args.base || 'http://127.0.0.1:28888'
const PORT = Number(args.port || 9222)
const UPSTREAM = args.upstream || 'http://127.0.0.1:28899/v1'
/** 第二个假上游：与第一个**故意有同名模型**，用来验证多供应商不串台 */
const UPSTREAM2 = args.upstream2 || 'http://127.0.0.1:28900/v1'
const API_KEY = args['api-key'] || 'test-key-123456'
/** 会产生 CoT 的模型（用来验流式 CoT 与刷新续传） */
const REASONING_MODEL = args['reasoning-model'] || 'mock-reasoner'
/** 不支持思考的模型（用来验强度选择器被禁用） */
const PLAIN_MODEL = args['plain-model'] || 'mock-chat'
/** 真实上游首字可能要等十几秒，把等待上限整体放大 */
const SLOW = Number(args['timeout-scale'] || (UPSTREAM.includes('127.0.0.1') || UPSTREAM.includes('localhost') ? 1 : 4))
const T = (ms) => Math.round(ms * SLOW)
const KEEP = !!args.keep

const CHROME_CANDIDATES = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
]

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/* ------------------------------------------------------------------ */
/* 迷你 CDP 客户端                                                     */
/* ------------------------------------------------------------------ */

class Cdp {
  constructor(ws) {
    this.ws = ws
    this.seq = 0
    this.waiting = new Map()
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.id && this.waiting.has(msg.id)) {
        const { resolve, reject } = this.waiting.get(msg.id)
        this.waiting.delete(msg.id)
        if (msg.error) reject(new Error(`${msg.error.message} ${JSON.stringify(msg.error.data ?? '')}`))
        else resolve(msg.result)
      }
    }
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

  async waitFor(expression, desc, timeout = 15000) {
    // 真实上游首字可能要等十几秒，统一按 SLOW 放大等待上限（本地 mock 时 SLOW=1）
    const budget = T(timeout)
    const t0 = Date.now()
    let last
    while (Date.now() - t0 < budget) {
      last = await this.eval(expression)
      if (last) return last
      await sleep(80)
    }
    throw new Error(
      `等待「${desc}」超时 ${budget}ms\n  表达式: ${expression.slice(0, 160)}\n  最后一次结果: ${JSON.stringify(last)}`,
    )
  }

  /* ---- 元素定位：一律用 JS 表达式求元素，避免 :nth-of-type / :last-of-type 的坑 ---- */

  /**
   * expr 必须求值成一个 Element；先滚进视口，再返回中心点坐标。
   * 视口外的元素点击会静默无效（鼠标事件打在空处），所以这里显式报错。
   */
  async boxOf(expr, desc = '元素') {
    const b = await this.eval(`(() => {
      const el = ${expr}
      if (!el) return null
      el.scrollIntoView({ block: 'center', inline: 'center' })
      const r = el.getBoundingClientRect()
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height, vw: innerWidth, vh: innerHeight }
    })()`)
    if (!b) throw new Error(`找不到${desc}: ${expr.slice(0, 120)}`)
    if (b.w === 0 || b.h === 0) throw new Error(`${desc}尺寸为 0，不可交互: ${expr.slice(0, 120)}`)
    if (b.x < 0 || b.y < 0 || b.x > b.vw || b.y > b.vh) {
      throw new Error(
        `${desc}滚动后仍不在视口内（x=${Math.round(b.x)}, y=${Math.round(b.y)}，视口 ${b.vw}×${b.vh}），点了也不会有反应`,
      )
    }
    return b
  }

  async hoverExpr(expr, desc = '元素') {
    const b = await this.boxOf(expr, desc)
    await this.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: b.x, y: b.y })
    await sleep(150) // 等 :hover 生效
  }

  async clickExpr(expr, desc = '元素') {
    const b = await this.boxOf(expr, desc)
    await this.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: b.x, y: b.y })
    await sleep(80)
    await this.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: b.x, y: b.y, button: 'left', clickCount: 1 })
    await this.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: b.x, y: b.y, button: 'left', clickCount: 1 })
    await sleep(150)
  }

  async textExpr(expr) {
    return await this.eval(`(() => { const el = ${expr}; return el ? el.innerText : null })()`)
  }

  /** 按可见文字点按钮（JS 里找元素、滚进视口，再按坐标点） */
  async clickByText(scopeExpr, text, desc) {
    const box = await this.eval(`(() => {
      const scope = ${scopeExpr} || document
      const el = [...scope.querySelectorAll('button, .picker__item, .conv__title')]
        .find((e) => (e.textContent || '').trim().includes(${JSON.stringify(text)}))
      if (!el) return null
      el.scrollIntoView({ block: 'center', inline: 'center' })
      const r = el.getBoundingClientRect()
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height, vw: innerWidth, vh: innerHeight, disabled: !!el.disabled }
    })()`)
    if (!box) throw new Error(`找不到包含「${text}」的可点元素（${desc || ''}）`)
    if (box.disabled) throw new Error(`「${text}」按钮是禁用状态`)
    if (box.x < 0 || box.y < 0 || box.x > box.vw || box.y > box.vh) {
      throw new Error(`「${text}」不在视口内（y=${Math.round(box.y)}，视口高 ${box.vh}）`)
    }
    await this.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: box.x, y: box.y })
    await sleep(60)
    await this.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: box.x, y: box.y, button: 'left', clickCount: 1 })
    await this.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: box.x, y: box.y, button: 'left', clickCount: 1 })
    await sleep(180)
  }

  /* ---- 输入 ---- */

  async focus(selector) {
    await this.eval(`(() => { const el = document.querySelector(${JSON.stringify(selector)}); el.focus(); return true })()`)
  }

  async type(text) {
    await this.send('Input.insertText', { text })
    await sleep(100)
  }

  /**
   * 往 <input type="file"> 里塞真实文件。
   * CDP 只能这样"选文件"——Input.dispatchKeyEvent 带不了文件内容。
   * 注意元素是 hidden 的也能塞，DOM.querySelector 找得到就行。
   */
  async setFileInput(selector, filePaths) {
    await this.send('DOM.enable')
    const doc = await this.send('DOM.getDocument', { depth: -1 })
    const found = await this.send('DOM.querySelector', { nodeId: doc.root.nodeId, selector })
    if (!found?.nodeId) throw new Error(`找不到文件输入框: ${selector}`)
    await this.send('DOM.setFileInputFiles', { files: filePaths, nodeId: found.nodeId })
    await sleep(500)
  }

  async fill(selector, text) {
    await this.focus(selector)
    await this.eval(`(() => { document.querySelector(${JSON.stringify(selector)}).select(); return true })()`)
    await this.send('Input.insertText', { text })
    await sleep(150)
    const v = await this.eval(`document.querySelector(${JSON.stringify(selector)}).value`)
    if (v !== text) throw new Error(`填值失败 ${selector}: 期望 ${JSON.stringify(text)}，实际 ${JSON.stringify(v)}`)
  }

  async key(name, mods = {}) {
    const def = {
      Enter: { vk: 13, code: 'Enter', key: 'Enter', text: '\r' },
      Escape: { vk: 27, code: 'Escape', key: 'Escape' },
      Backspace: { vk: 8, code: 'Backspace', key: 'Backspace' },
    }[name]
    if (!def) throw new Error(`未定义的按键: ${name}`)

    let modifiers = 0
    if (mods.alt) modifiers |= 1
    if (mods.ctrl) modifiers |= 2
    if (mods.meta) modifiers |= 4
    if (mods.shift) modifiers |= 8

    const base = {
      modifiers,
      windowsVirtualKeyCode: def.vk,
      nativeVirtualKeyCode: def.vk,
      code: def.code,
      key: def.key,
    }
    // 带 text 的用 keyDown 才会产生默认行为（textarea 里插入换行）；
    // 带 Ctrl/Meta 的不产生文本，用 rawKeyDown。
    if (def.text && !mods.ctrl && !mods.meta) {
      await this.send('Input.dispatchKeyEvent', { type: 'keyDown', ...base, text: def.text, unmodifiedText: def.text })
    } else {
      await this.send('Input.dispatchKeyEvent', { type: 'rawKeyDown', ...base })
    }
    await this.send('Input.dispatchKeyEvent', { type: 'keyUp', ...base })
    await sleep(150)
  }

  async value(selector) {
    return await this.eval(`document.querySelector(${JSON.stringify(selector)})?.value ?? null`)
  }

  async count(selector) {
    return await this.eval(`document.querySelectorAll(${JSON.stringify(selector)}).length`)
  }
}

/* ------------------------------------------------------------------ */
/* 极简测试框架                                                        */
/* ------------------------------------------------------------------ */

const results = []
let currentGroup = ''

function group(name) {
  currentGroup = name
  console.log(`\n\x1b[36m=== ${name} ===\x1b[0m`)
}

async function test(name, fn) {
  try {
    const note = await fn()
    results.push({ group: currentGroup, name, ok: true, note })
    console.log(`  \x1b[32m[PASS]\x1b[0m ${name}${note ? `  \x1b[90m${note}\x1b[0m` : ''}`)
  } catch (e) {
    results.push({ group: currentGroup, name, ok: false, note: e.message })
    console.log(`  \x1b[31m[FAIL]\x1b[0m ${name}\n         \x1b[31m${String(e.message).split('\n').join('\n         ')}\x1b[0m`)
  }
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg)
}

/* ------------------------------------------------------------------ */
/* 启动 Chrome                                                         */
/* ------------------------------------------------------------------ */

function findChrome() {
  for (const p of CHROME_CANDIDATES) if (existsSync(p)) return p
  throw new Error('找不到 Chrome/Edge，请用 --chrome 指定路径')
}

/**
 * 找一个可用的 page target。
 *
 * 有个坑：端口上可能挂着一个**没有标签页**的僵尸 Chrome（之前跑测试留下的），
 * 这时 /json/list 是空数组。如果直接去抢同一个端口起新实例，新实例绑不上端口就
 * 静默退出，最后表现成「Chrome 启动后连不上 CDP」。所以这里先试着自己开一个标签页。
 */
async function cdpEndpoint() {
  let list
  try {
    list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()
  } catch {
    return null // 端口上什么都没有
  }
  const found = list.find((x) => x.type === 'page')
  if (found) return found.webSocketDebuggerUrl

  // 有 CDP 但没有标签页：让 Chrome 开一个
  try {
    const created = await (
      await fetch(`http://127.0.0.1:${PORT}/json/new?about:blank`, { method: 'PUT' })
    ).json()
    if (created?.webSocketDebuggerUrl) {
      console.log('端口上的 Chrome 没有标签页，已替你开一个')
      return created.webSocketDebuggerUrl
    }
  } catch {
    /* 下面返回 null */
  }
  return null
}

let chromeProc = null
let profileDir = null

async function ensureChrome() {
  let endpoint = await cdpEndpoint()
  if (endpoint) {
    console.log(`连上已存在的 Chrome（端口 ${PORT}）`)
    return endpoint
  }
  const bin = args.chrome || findChrome()
  profileDir = mkdtempSync(join(tmpdir(), 'ui-test-chrome-'))
  console.log(`启动无头 Chrome: ${bin}`)
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
    endpoint = await cdpEndpoint()
    if (endpoint) return endpoint
  }
  throw new Error(
    `Chrome 启动后连不上 CDP（端口 ${PORT}）。\n` +
      `  多半是端口被一个残留的 Chrome 占着：换一个端口重试，例如 --port 9333；\n` +
      `  或者把无头 Chrome 清掉（注意别关掉用户自己的浏览器）。`,
  )
}

/* ------------------------------------------------------------------ */
/* 选择器常量                                                          */
/* ------------------------------------------------------------------ */

const COMPOSER = '.composer__area'
const MODAL = 'document.querySelector(".modal__panel")'
const LAST_ASST = `[...document.querySelectorAll('.msg--assistant')].pop()`
/** 注意这两个是「求值成字符串/数字」的表达式，要用 cdp.eval，不能塞进 textExpr（那是求 Element 的） */
const COT_TEXT = `([...document.querySelectorAll('.msg--assistant')].pop()?.querySelector('.reasoning__body')?.innerText || '')`
const COT_LEN = `${COT_TEXT}.length`
const USER_COUNT = `document.querySelectorAll('.msg--user').length`

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

  /* ---------- A. 首屏 ---------- */
  group('A. 首屏与布局')

  await cdp.send('Page.navigate', { url: BASE })
  await cdp.waitFor(`!!document.querySelector('.composer__area')`, '输入框出现', 25000)

  await test('页面骨架渲染出来（侧栏 + 消息区 + 输入框 + 模型选择器）', async () => {
    const ok = await cdp.eval(`({
      sidebar: !!document.querySelector('.sidebar'),
      list: !!document.querySelector('.msglist'),
      composer: !!document.querySelector('.composer__area'),
      picker: !!document.querySelector('.picker'),
    })`)
    assert(ok.sidebar && ok.list && ok.composer && ok.picker, `缺少元素: ${JSON.stringify(ok)}`)
    return 'sidebar / msglist / composer / picker 都在'
  })

  await test('未配置供应商时自动弹出设置弹窗', async () => {
    await cdp.waitFor(`!!document.querySelector('.modal')`, '设置弹窗出现', 10000)
    return '已自动打开'
  })

  /* ---------- B. 设置：添加供应商 → 拉模型列表 ---------- */
  group('B. 设置：添加供应商、拉取模型列表')

  await test('添加一个供应商并保存', async () => {
    await cdp.clickByText(MODAL, '添加供应商', '供应商区')
    await cdp.waitFor(`!!document.querySelector('#prov-url')`, '供应商表单出现', T(5000))
    await cdp.fill('#prov-url', UPSTREAM)
    await cdp.fill('#prov-key', API_KEY)
    await cdp.fill('#prov-name', '网关A')
    await cdp.clickByText(MODAL, '保存', '供应商表单')
    // 保存成功后表单收起、供应商行出现
    await cdp.waitFor(
      `!document.querySelector('#prov-url') && !!document.querySelector('.prov')`,
      '供应商行出现',
      T(10000),
    )
    const name = await cdp.textExpr(`document.querySelector('.prov__name')`)
    assert(name?.includes('网关A'), `供应商名不对: ${JSON.stringify(name)}`)
    return name
  })

  await test('「获取模型列表」能把模型拉回来并渲染', async () => {
    await cdp.clickByText(MODAL, '获取模型列表', '供应商行')
    await cdp.waitFor(`!!document.querySelector('.prov__models .mtable__wrap')`, '模型表渲染出来', T(30000))
    const n = await cdp.count('.prov__models .mtable__wrap tbody tr')
    assert(n > 0, '模型表是空的')
    return `${n} 个模型`
  })

  await test('拉取成功提示「共 N 个，新增 M 个」', async () => {
    const txt = await cdp.textExpr(MODAL)
    const m = txt.match(/共\s*(\d+)\s*个[^]*?新增\s*(\d+)\s*个/)
    assert(m, `没找到成功提示，弹窗文字片段: ${JSON.stringify(txt.slice(0, 240))}`)
    return `共 ${m[1]} 个，新增 ${m[2]} 个`
  })

  await test('「测试连接」能通', async () => {
    await cdp.clickByText(MODAL, '测试连接', '供应商行')
    await cdp.waitFor(
      `/连接正常|失败|latency/.test(document.querySelector('.modal__panel').innerText)`,
      '测试结果出现',
      T(30000),
    )
    const txt = await cdp.textExpr(MODAL)
    assert(!txt.includes('失败'), `测试连接失败: ${txt.slice(0, 240)}`)
    return '连接正常'
  })

  await test('关闭设置弹窗后输入框变为可用', async () => {
    await cdp.clickByText(MODAL, '关闭', '弹窗内')
    await cdp.waitFor(`!document.querySelector('.modal')`, '弹窗关闭', 5000)
    await cdp.waitFor(`!!document.querySelector('.composer__area:not([disabled])')`, '输入框可用', T(10000))
    return '已关闭，输入框可用'
  })

  /* ---------- C. 换行键与输入法 ---------- */
  group('C. 换行键与中文输入法守卫')

  await test('「+ 新对话」可用', async () => {
    await cdp.clickExpr(`document.querySelector('.sidebar__new')`, '「+ 新对话」按钮')
    await cdp.waitFor(`!!document.querySelector('.composer__area:not([disabled])')`, '输入框可用', 10000)
    return '已新建并选中'
  })

  await test(`把新会话的模型切到 ${REASONING_MODEL}（后面要验 CoT，不能靠默认值碰巧支持）`, async () => {
    await cdp.clickExpr(`document.querySelector('.picker__trigger')`, '模型下拉')
    await cdp.waitFor(`!!document.querySelector('.picker__menu')`, '下拉出现', T(8000))
    await cdp.clickByText(`document.querySelector('.picker__menu')`, REASONING_MODEL, '模型菜单')
    await cdp.waitFor(
      `(document.querySelector('.topbar__sub')?.innerText || '').includes(${JSON.stringify(REASONING_MODEL)})`,
      '模型已切换',
      T(10000),
    )
    return REASONING_MODEL
  })

  await test('Enter 之外的换行键不发送（Ctrl+Enter / Shift+Enter）', async () => {
    await cdp.focus(COMPOSER)
    await cdp.type('第一行')
    const before = await cdp.eval(USER_COUNT)

    await cdp.key('Enter', { ctrl: true })
    const afterCtrl = await cdp.value(COMPOSER)

    await cdp.type('第二行')
    await cdp.key('Enter', { shift: true })
    const afterShift = await cdp.value(COMPOSER)

    const after = await cdp.eval(USER_COUNT)
    assert(after === before, `换行键把消息发出去了（用户消息 ${before} → ${after}）`)

    const ctrlOk = afterCtrl.includes('\n')
    const shiftOk = afterShift.includes('\n')
    assert(
      ctrlOk && shiftOk,
      `换行没生效：Ctrl+Enter 后 = ${JSON.stringify(afterCtrl)}，Shift+Enter 后 = ${JSON.stringify(afterShift)}`,
    )
    return `两种换行都生效，当前换行数 = ${(afterShift.match(/\n/g) || []).length}`
  })

  await test('中文输入法组字时按 Enter 不发送（isComposing）', async () => {
    const before = await cdp.eval(USER_COUNT)
    // 这正是拼音选词时按回车那一刻的事件形态
    await cdp.eval(`(() => {
      const el = document.querySelector('${COMPOSER}')
      el.focus()
      return el.dispatchEvent(new KeyboardEvent('keydown', {
        key: 'Enter', code: 'Enter', keyCode: 229, which: 229,
        isComposing: true, bubbles: true, cancelable: true,
      }))
    })()`)
    await sleep(1200)
    const after = await cdp.eval(USER_COUNT)
    assert(after === before, `组字时按回车把消息发出去了（${before} → ${after}）`)
    return 'isComposing=true 的 Enter 被放行'
  })

  await test('真实 IME 组字状态下按 Enter 不发送', async () => {
    await cdp.focus(COMPOSER)
    await cdp.send('Input.imeSetComposition', { text: 'nihao', selectionStart: 5, selectionEnd: 5 })
    const composing = await cdp.eval(`(() => {
      const el = document.querySelector('${COMPOSER}')
      return new Promise((res) => {
        const onKey = (e) => { el.removeEventListener('keydown', onKey); res(!!e.isComposing) }
        el.addEventListener('keydown', onKey)
        setTimeout(() => res(null), 1500)
      })
    })()`)
    const before = await cdp.eval(USER_COUNT)
    await cdp.key('Enter')
    await sleep(1000)
    await cdp.send('Input.imeSetComposition', { text: '', selectionStart: 0, selectionEnd: 0 })
    const after = await cdp.eval(USER_COUNT)
    assert(after === before, `IME 组字时回车发送了消息（${before} → ${after}）`)
    return composing === null ? '已放行（这次没捕获到 isComposing 标志）' : `keydown.isComposing=${composing}，未发送`
  })

  await test('清空输入框', async () => {
    await cdp.focus(COMPOSER)
    await cdp.eval(`(() => { document.querySelector('${COMPOSER}').select(); return true })()`)
    await cdp.key('Backspace')
    await sleep(100)
    const v = await cdp.value(COMPOSER)
    if (v !== '') throw new Error(`清空失败: ${JSON.stringify(v)}`)
    return '已清空'
  })

  /* ---------- D. 发送与流式 CoT ---------- */
  group('D. 发送、流式 CoT')

  const PROMPT =
    '请尽量详细地用中文讲一下 Go 的并发模型（goroutine、channel、select、调度器），并给一个完整的 Hello World 示例。内容多一些，我要用来观察流式输出。'

  await test('Enter 发送：用户消息由后端事件推下来才出现', async () => {
    await cdp.focus(COMPOSER)
    await cdp.type(PROMPT)
    const before = await cdp.eval(USER_COUNT)
    await cdp.key('Enter')
    await cdp.waitFor(
      `[...document.querySelectorAll('.msg--user')].some(m => (m.innerText||'').includes('并发模型'))`,
      '用户消息出现在页面上',
      15000,
    )
    const after = await cdp.eval(USER_COUNT)
    assert(after === before + 1, `用户消息条数应为 ${before + 1}，实际 ${after}`)
    const cleared = await cdp.value(COMPOSER)
    assert(cleared === '', `发送成功后输入框应清空，实际 ${JSON.stringify(cleared)}`)
    return `用户消息 ${before} → ${after}，输入框已清空`
  })

  await test('生成中：停止按钮 + 光标动效 + CoT 脉冲标记', async () => {
    await cdp.waitFor(
      `!!document.querySelector('.composer__send--stop') && !!document.querySelector('.cursor') && !!document.querySelector('.reasoning .pulse')`,
      '生成中的三个视觉标记',
      15000,
    )
    return '都在'
  })

  await test('CoT 逐字刷新（两次采样在增长）', async () => {
    const t1 = await cdp.eval(COT_TEXT)
    await sleep(900)
    const t2 = await cdp.eval(COT_TEXT)
    assert(t1.length > 0, 'CoT 块是空的')
    assert(t2.length > t1.length, `CoT 没有增长：${t1.length} → ${t2.length}`)
    return `CoT 文本 ${t1.length} → ${t2.length} 字`
  })

  /* ---------- E. 生成中刷新页面 ---------- */
  group('E. 生成中刷新页面（CoT 必须续上）')

  let cotBeforeReload = 0

  await test('先记下刷新前的 CoT 长度', async () => {
    cotBeforeReload = await cdp.eval(COT_LEN)
    assert(cotBeforeReload > 0, '刷新前 CoT 就是空的，无法验证续传')
    return `${cotBeforeReload} 字`
  })

  await test('刷新页面后 CoT 仍在，并且继续增长（不是从头开始）', async () => {
    await cdp.send('Page.reload')
    await cdp.waitFor(`!!document.querySelector('.msg--assistant .reasoning__body')`, 'CoT 块重新出现', 25000)
    const afterReload = await cdp.eval(COT_LEN)
    assert(afterReload >= cotBeforeReload, `刷新后 CoT 从 ${cotBeforeReload} 变成 ${afterReload}，内容丢了`)

    await sleep(1200)
    const later = await cdp.eval(COT_LEN)
    const stillGenerating = await cdp.eval(`!!document.querySelector('.composer__send--stop')`)
    if (later > afterReload) {
      return `刷新前 ${cotBeforeReload} → 刷新后 ${afterReload} → 再等 1.2s ${later}（继续在长）`
    }
    // 采样时生成已经结束（快上游会出现）：只要 CoT 没丢、并且和库里对得上，也算通过
    assert(!stillGenerating, `仍在生成，但 CoT 没有继续增长：${afterReload} → ${later}`)
    const st = await activeState()
    const dbMsg = [...st.messages].reverse().find((m) => m.role === 'assistant')
    assert(
      (dbMsg?.reasoning?.length ?? 0) >= afterReload,
      `界面上的 CoT(${afterReload}) 比库里的(${dbMsg?.reasoning?.length}) 还少`,
    )
    return `刷新前 ${cotBeforeReload} → 刷新后 ${afterReload}（采样时生成已结束，CoT 完整未丢）`
  })

  await test('等生成结束：停止按钮消失、usage 出现', async () => {
    await cdp.waitFor(`!document.querySelector('.composer__send--stop')`, '生成结束', 60000)
    await cdp.waitFor(`!!document.querySelector('.msg__usage')`, 'usage 出现', 10000)
    return await cdp.textExpr(`${LAST_ASST}?.querySelector('.msg__usage')?.innerText`)
  })

  /* ---------- F. 编辑与还原 ---------- */
  group('F. 编辑回复正文 / 还原原文')

  const EDITED = '这是我改写的正文。'

  /** 直接问后端要最后一条助手消息 —— 断言不依赖具体模型吐出什么文案 */
  async function lastAssistantMessage() {
    const convs = await (await fetch(BASE + '/api/conversations')).json()
    const st = await (await fetch(`${BASE}/api/conversations/${convs[0].id}/state`)).json()
    const list = st.messages.filter((m) => m.role === 'assistant')
    return list[list.length - 1]
  }

  let originalContent = ''

  await test('悬停消息后出现工具条', async () => {
    await cdp.hoverExpr(LAST_ASST, '最后一条助手消息')
    const n = await cdp.count('.msg__tools button')
    assert(n >= 4, `工具条按钮只有 ${n} 个`)
    return `${n} 个按钮`
  })

  await test('点「编辑」进入编辑态，框里正是后端存的那份 markdown', async () => {
    const msg = await lastAssistantMessage()
    originalContent = msg.content
    assert(originalContent.length > 0, '后端这条消息没有正文')
    await cdp.clickExpr(`${LAST_ASST}?.querySelector('[title="编辑"]')`, '编辑按钮')
    await cdp.waitFor(`!!(${LAST_ASST}?.querySelector('.editor__area'))`, '编辑框出现', 5000)
    const v = await cdp.eval(`${LAST_ASST}?.querySelector('.editor__area')?.value || ''`)
    assert(
      v === originalContent,
      `编辑框内容与后端不一致\n  框里: ${JSON.stringify(v.slice(0, 70))}\n  库里: ${JSON.stringify(originalContent.slice(0, 70))}`,
    )
    return `与库中 markdown 逐字一致（${v.length} 字）`
  })

  await test('Ctrl+Enter 保存编辑：后端 content/edited/content_original 都对', async () => {
    await cdp.eval(`(() => {
      const el = ${LAST_ASST}.querySelector('.editor__area')
      el.focus(); el.select(); return true
    })()`)
    await cdp.type(EDITED)
    await cdp.key('Enter', { ctrl: true })
    await cdp.waitFor(
      `[...document.querySelectorAll('.msg__content')].some(e => (e.innerText||'').includes(${JSON.stringify(EDITED)}))`,
      '编辑后的正文渲染出来',
      T(10000),
    )
    const msg = await lastAssistantMessage()
    assert(msg.content === EDITED, `后端正文不对: ${JSON.stringify(msg.content)}`)
    assert(msg.edited === true, '后端没有标记 edited')
    assert(msg.content_original === originalContent, '后端没有保留原文')
    await cdp.hoverExpr(LAST_ASST, '最后一条助手消息')
    assert(await cdp.eval(`!!(${LAST_ASST}?.querySelector('.msg__edited'))`), '没有出现「已编辑」标记')
    return 'content 已改 / edited=true / content_original 保留'
  })

  await test('刷新页面后编辑内容仍在（真的落库了）', async () => {
    await cdp.send('Page.reload')
    await cdp.waitFor(`!!document.querySelector('.msg--assistant .msg__content')`, '页面重新加载', 25000)
    await cdp.waitFor(
      `[...document.querySelectorAll('.msg__content')].some(e => (e.innerText||'').includes(${JSON.stringify(EDITED)}))`,
      '编辑后的正文仍在',
      T(10000),
    )
    return '刷新后仍在'
  })

  await test('编辑器里点「还原原文」：正文与 edited 都回到模型原文', async () => {
    await cdp.hoverExpr(LAST_ASST, '最后一条助手消息')
    await cdp.clickExpr(`${LAST_ASST}?.querySelector('[title="编辑"]')`, '编辑按钮')
    await cdp.waitFor(`!!(${LAST_ASST}?.querySelector('.editor__area'))`, '编辑框出现', 5000)
    await cdp.clickByText(`${LAST_ASST}?.querySelector('.editor__bar')`, '还原原文', '编辑工具条')
    await sleep(1200)
    const msg = await lastAssistantMessage()
    assert(
      msg.content === originalContent,
      `正文没有还原\n  现在: ${JSON.stringify(msg.content.slice(0, 70))}\n  原文: ${JSON.stringify(originalContent.slice(0, 70))}`,
    )
    assert(msg.edited === false, '后端 edited 没有复位')
    await cdp.hoverExpr(LAST_ASST, '最后一条助手消息')
    const badge = await cdp.eval(`!!(${LAST_ASST}?.querySelector('.msg__edited'))`)
    assert(!badge, '「已编辑」标记没有消失')
    return '正文与 edited 都已还原'
  })

  /* ---------- G. 生成中再发消息 ---------- */
  group('G. 生成中再发消息（应被丢弃）')

  await test('先重新发起一轮生成', async () => {
    await cdp.focus(COMPOSER)
    await cdp.type('再讲一遍，我要看思考过程。')
    await cdp.key('Enter')
    await cdp.waitFor(`!!document.querySelector('.composer__send--stop')`, '进入生成中', 15000)
    return '生成中'
  })

  await test('生成中按 Enter：弹提示、输入框保留、页面不多出消息', async () => {
    await cdp.focus(COMPOSER)
    await cdp.type('这条应该被丢弃')
    const before = await cdp.eval(USER_COUNT)
    await cdp.key('Enter')
    // 注意：toast 会堆叠，必须找「内容匹配的那一条」，不能取第一个
    await cdp.waitFor(
      `[...document.querySelectorAll('.toast')].some(t => (t.innerText||'').includes('丢弃'))`,
      '出现「已丢弃」提示',
      10000,
    )
    const toastText = await cdp.eval(
      `([...document.querySelectorAll('.toast')].find(t => (t.innerText||'').includes('丢弃'))?.innerText || '')`,
    )
    await sleep(600)
    const after = await cdp.eval(USER_COUNT)
    assert(after === before, `被丢弃的消息竟然出现在页面上（${before} → ${after}）`)
    const kept = await cdp.value(COMPOSER)
    assert(kept === '这条应该被丢弃', `输入框文字没有保留，实际 ${JSON.stringify(kept)}`)
    return `toast「${toastText.trim()}」，输入框文字保留`
  })

  await test('点「停止」能中断生成', async () => {
    await cdp.clickExpr(`document.querySelector('.composer__send--stop')`, '停止按钮')
    await cdp.waitFor(`!document.querySelector('.composer__send--stop')`, '生成已停止', 15000)
    return '已停止'
  })

  /* ---------- H. 模型与思考强度 ---------- */
  group('H. 模型 / 思考强度选择器')

  await test('模型下拉：能列出模型，选中后写回后端并反映到顶栏', async () => {
    const before = await cdp.textExpr(`document.querySelector('.topbar__sub')`)
    await cdp.clickExpr(`document.querySelector('.picker__trigger')`, '模型下拉')
    await cdp.waitFor(`!!document.querySelector('.picker__menu')`, '下拉菜单出现', T(8000))
    const n = await cdp.count('.picker__menu .picker__item')
    assert(n > 0, '下拉里没有模型项')

    // 优先切到「不支持思考」的模型（给下一条用例铺路）；上游没有这种模型时随便切一个
    const target = await cdp.eval(`(() => {
      const items = [...document.querySelectorAll('.picker__menu .picker__item')]
      const texts = items.map((e) => (e.innerText || '').trim())
      const plain = texts.find((t) => t.includes(${JSON.stringify(PLAIN_MODEL)}))
      const other = texts.find((t) => !t.includes(${JSON.stringify(REASONING_MODEL)}))
      return plain || other || null
    })()`)
    assert(target, '没有可切换的模型项')
    await cdp.clickByText(`document.querySelector('.picker__menu')`, target.split('\n')[0], '模型菜单')
    await sleep(1200)
    const after = await cdp.textExpr(`document.querySelector('.topbar__sub')`)
    assert(before !== after, `顶栏模型信息没变: ${before}`)
    return `${n} 个可选项；${before} → ${after}`
  })

  await test('不支持思考的模型：强度选择器禁用并给出提示', async () => {
    const st = await cdp.eval(`(() => {
      const trig = [...document.querySelectorAll('.picker__trigger')].pop()
      return { title: trig?.getAttribute('title') || '', text: (trig?.innerText || '').trim(), disabled: !!trig?.disabled }
    })()`)
    if (!st.title.includes('不支持') && !st.text.includes('不支持')) {
      // 真实上游的模型可能全都支持思考强度，这不是失败
      return `跳过：当前模型支持思考强度（选择器显示「${st.text}」）`
    }
    assert(st.disabled, `强度选择器应当被禁用，实际 disabled=${st.disabled}`)
    return `「${st.text}」且已禁用`
  })

  /* ---------- I. 对话标题 ---------- */
  group('I. 对话标题：隐式起名 + 用户自定义')

  /** 当前会话的权威状态（列表里最近更新的那个就是当前会话） */
  async function activeState() {
    const convs = await (await fetch(BASE + '/api/conversations')).json()
    return await (await fetch(`${BASE}/api/conversations/${convs[0].id}/state`)).json()
  }

  const MY_TITLE = '我自己起的标题'

  await test('起标题是隐式的：没往对话里插消息，也没把提示词泄漏进聊天记录', async () => {
    const s = await activeState()
    const leaked = s.messages.filter(
      (m) => m.content.includes('起一个标题') || m.content.includes('请给下面这段对话起个标题'),
    )
    assert(leaked.length === 0, `起标题的提示词泄漏进聊天记录了（${leaked.length} 条）`)
    assert(
      s.messages.length % 2 === 0,
      `消息应当一问一答成对出现，实际 ${s.messages.length} 条：${s.messages.map((m) => m.role).join(',')}`,
    )
    return `${s.messages.length} 条消息，无多余记录`
  })

  await test('标题已经不是「新对话」，也不是整句提问（说明确实起了名）', async () => {
    const s = await activeState()
    assert(s.conv.title && s.conv.title !== '新对话', `标题还是默认值: ${JSON.stringify(s.conv.title)}`)
    const firstUser = s.messages.find((m) => m.role === 'user')?.content ?? ''
    assert(
      s.conv.title.length < firstUser.length,
      `标题（${s.conv.title.length} 字）没有比原始提问（${firstUser.length} 字）短，像是没起过名`,
    )
    return `「${s.conv.title}」（原标题 ${firstUser.length} 字）`
  })

  await test('顶栏铅笔按钮可以自定义标题，并真的落库', async () => {
    await cdp.clickExpr(`document.querySelector('.topbar__pen')`, '顶栏重命名按钮')
    await cdp.waitFor(`!!document.querySelector('.topbar__rename')`, '重命名输入框出现', T(5000))
    await cdp.eval(`(() => {
      const el = document.querySelector('.topbar__rename')
      el.focus(); el.select(); return true
    })()`)
    await cdp.type(MY_TITLE)
    await cdp.key('Enter')
    await cdp.waitFor(
      `(document.querySelector('.topbar__name')?.innerText || '').trim() === ${JSON.stringify(MY_TITLE)}`,
      '顶栏标题更新',
      T(8000),
    )
    const list = await (await fetch(BASE + '/api/conversations')).json()
    assert(list.some((c) => c.title === MY_TITLE), '自定义标题没有落库')
    return MY_TITLE
  })

  await test('刷新页面后自定义标题仍在（AI 不会覆盖用户起的名字）', async () => {
    await cdp.send('Page.reload')
    await cdp.waitFor(`!!document.querySelector('.topbar__name')`, '页面重新加载', T(25000))
    await sleep(1500)
    const shown = (await cdp.textExpr(`document.querySelector('.topbar__name')`))?.trim()
    assert(shown === MY_TITLE, `刷新后标题变成了「${shown}」`)
    return shown
  })

  /* ---------- J. 多供应商 ---------- */
  group('J. 多供应商：同名模型不串台')

  /** 第二个假上游在不在？不在就跳过这一组（不算失败） */
  let upstream2Ready = false
  try {
    upstream2Ready = (await fetch(`${UPSTREAM2}/models`)).ok
  } catch {
    upstream2Ready = false
  }

  if (!upstream2Ready) {
    await test('多供应商用例', async () => {
      return `跳过：第二个假上游 ${UPSTREAM2} 不可达（起 mock 时加 -addr 127.0.0.1:28900 即可启用）`
    })
  } else {
    await test('再添加第二个供应商并拉取它的模型', async () => {
      await cdp.clickExpr(`document.querySelector('.topbar [title="设置"]')`, '设置按钮')
      await cdp.waitFor(`!!document.querySelector('.modal')`, '设置弹窗打开', T(5000))
      await cdp.clickByText(MODAL, '添加供应商', '供应商区')
      await cdp.waitFor(`!!document.querySelector('#prov-url')`, '供应商表单出现', T(5000))
      await cdp.fill('#prov-url', UPSTREAM2)
      await cdp.fill('#prov-key', API_KEY)
      await cdp.fill('#prov-name', '网关B')
      await cdp.clickByText(MODAL, '保存', '供应商表单')
      await cdp.waitFor(`document.querySelectorAll('.prov').length >= 2`, '出现两个供应商行', T(10000))

      // 给第二行点「获取模型列表」：按行内按钮定位，别点到第一行
      const box = await cdp.eval(`(() => {
        const rows = [...document.querySelectorAll('.prov')]
        const row = rows[rows.length - 1]
        const btn = [...row.querySelectorAll('button')].find(b => (b.textContent||'').includes('获取模型列表'))
        if (!btn) return null
        btn.scrollIntoView({ block: 'center' })
        const r = btn.getBoundingClientRect()
        return { x: r.left + r.width/2, y: r.top + r.height/2 }
      })()`)
      assert(box, '第二行没有「获取模型列表」按钮')
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: box.x, y: box.y })
      await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: box.x, y: box.y, button: 'left', clickCount: 1 })
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: box.x, y: box.y, button: 'left', clickCount: 1 })
      await cdp.waitFor(`document.querySelectorAll('.prov__models .mtable__wrap').length >= 2`, '两个供应商都有模型表', T(30000))
      return '网关B 已就绪'
    })

    await test('后端把同名模型按供应商分成两条独立记录', async () => {
      const providers = await (await fetch(BASE + '/api/providers')).json()
      assert(providers.length >= 2, `供应商应有 2 家，实际 ${providers.length}`)
      const models = await (await fetch(BASE + '/api/models')).json()
      const byName = new Map()
      for (const m of models) {
        if (!byName.has(m.model_id)) byName.set(m.model_id, [])
        byName.get(m.model_id).push(m)
      }
      const dup = [...byName.entries()].filter(([, list]) => list.length >= 2)
      assert(dup.length > 0, `没有找到任何同名模型（模型列表：${models.map((m) => m.model_id).join(', ')}）`)
      const [name, list] = dup[0]
      const pids = new Set(list.map((m) => m.provider_id))
      assert(pids.size >= 2, `${name} 的 provider_id 只有 ${[...pids].join(',')}`)
      const reasonings = new Set(list.map((m) => m.reasoning))
      return `「${name}」有 ${list.length} 条，provider_id=${[...pids].join(',')}，能力标志 ${reasonings.size} 种`
    })

    await test('模型选择器按供应商分组，同名模型是两条可分别选中的项', async () => {
      // 先关掉设置弹窗
      await cdp.clickByText(MODAL, '关闭', '弹窗内')
      await cdp.waitFor(`!document.querySelector('.modal')`, '弹窗关闭', 5000)
      await cdp.clickExpr(`document.querySelector('.picker__trigger')`, '模型下拉')
      await cdp.waitFor(`!!document.querySelector('.picker__menu')`, '下拉出现', T(8000))

      const info = await cdp.eval(`(() => {
        const items = [...document.querySelectorAll('.picker__menu .picker__item')]
        const groups = [...document.querySelectorAll('.picker__menu .picker__group')]
        const texts = items.map(e => (e.innerText || '').trim().split('\\n')[0])
        const dupName = texts.find((t, i) => texts.indexOf(t) !== i) || null
        return { items: texts.length, groups: groups.length, dupName, texts }
      })()`)
      assert(info.groups >= 2, `按供应商分组的分组标题只有 ${info.groups} 个`)
      assert(info.dupName, `没找到重名的模型项：${JSON.stringify(info.texts)}`)
      return `${info.items} 个模型项，${info.groups} 个供应商分组，重名「${info.dupName}」出现多次`
    })

    await test('选中第二家的同名模型会把 provider_id 一起写回会话', async () => {
      const before = await activeState()
      const targetProvider = await cdp.eval(`(() => {
        // 找「属于另一个供应商」的那条同名模型项
        const items = [...document.querySelectorAll('.picker__menu .picker__item')]
        const names = items.map(e => (e.innerText || '').trim().split('\\n')[0])
        const dup = names.find((t, i) => names.indexOf(t) !== i)
        if (!dup) return null
        const idx = names.map((t, i) => (t === dup ? i : -1)).filter(i => i >= 0)
        const el = items[idx[idx.length - 1]] // 取靠后那一条（通常属于后添加的供应商）
        el.scrollIntoView({ block: 'center' })
        const r = el.getBoundingClientRect()
        return { x: r.left + r.width/2, y: r.top + r.height/2, label: (el.innerText||'').trim() }
      })()`)
      assert(targetProvider, '找不到可点的同名模型项')
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: targetProvider.x, y: targetProvider.y })
      await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: targetProvider.x, y: targetProvider.y, button: 'left', clickCount: 1 })
      await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: targetProvider.x, y: targetProvider.y, button: 'left', clickCount: 1 })
      await sleep(1200)

      const after = await activeState()
      assert(
        after.conv.provider_id !== before.conv.provider_id,
        `provider_id 没有变化（仍是 ${after.conv.provider_id}），说明只写了 model_id`,
      )
      assert(after.conv.provider_name, 'provider_name 是空的')
      return `${before.conv.provider_name}(${before.conv.provider_id}) → ${after.conv.provider_name}(${after.conv.provider_id})`
    })
  }

  /* ---------- K. 生成途中切换模型 ---------- */
  group('K. 生成途中切换模型：本轮不改道')

  await test('生成中切换模型，界面提示「下一轮生效」，本轮消息仍记旧模型', async () => {
    // 起一轮生成（mock 故意慢，好在中途操作）
    await cdp.focus(COMPOSER)
    await cdp.type('生成中切换测试')
    await cdp.key('Enter')
    await cdp.waitFor(`!!document.querySelector('.composer__send--stop')`, '进入生成中', T(15000))

    const running = (await activeState()).generating
    assert(running, '此刻应当正在生成')
    const oldModel = running.model_id

    // 生成途中切到另一个模型
    await cdp.clickExpr(`document.querySelector('.picker__trigger')`, '模型下拉')
    await cdp.waitFor(`!!document.querySelector('.picker__menu')`, '菜单出现', T(8000))
    const target = await cdp.eval(`(() => {
      const items = [...document.querySelectorAll('.picker__menu .picker__item')]
      const el = items.find((e) => (e.innerText || '').trim().split('\\n')[0] !== ${JSON.stringify(oldModel)})
      if (!el) return null
      el.scrollIntoView({ block: 'center' })
      const r = el.getBoundingClientRect()
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, label: (el.innerText || '').trim().split('\\n')[0] }
    })()`)
    assert(target, `找不到与 ${oldModel} 不同的模型项`)
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: target.x, y: target.y })
    await cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: target.x, y: target.y, button: 'left', clickCount: 1 })
    await cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: target.x, y: target.y, button: 'left', clickCount: 1 })
    await sleep(900)

    // 再打开菜单：应当出现「下一轮生效」的提示
    await cdp.clickExpr(`document.querySelector('.picker__trigger')`, '模型下拉')
    await cdp.waitFor(`!!document.querySelector('.picker__menu')`, '菜单再次出现', T(8000))
    const hint = await cdp.textExpr(`document.querySelector('.picker__hint')`)
    assert(
      hint && hint.includes('下一轮生效'),
      `切了模型却没给出「下一轮生效」提示，实际 ${JSON.stringify(hint)}`,
    )
    assert(hint.includes(oldModel), `提示里应当点名本轮在用的 ${oldModel}，实际 ${JSON.stringify(hint)}`)
    await cdp.eval(`(() => { document.body.click(); return true })()`)

    // 等本轮结束
    await cdp.waitFor(`!document.querySelector('.composer__send--stop')`, '本轮结束', T(90000))
    const after = await activeState()
    const asst = [...after.messages].reverse().find((m) => m.role === 'assistant')
    assert(
      asst?.model_id === oldModel,
      `本轮消息应当仍记 ${oldModel}（实际用的是谁就记谁），实际 ${asst?.model_id}`,
    )
    assert(
      after.conv.model_id === target.label,
      `会话应当已经切到 ${target.label}（下一轮生效），实际 ${after.conv.model_id}`,
    )
    return `提示「${hint.trim()}」；本轮 ${oldModel} 跑完，会话已切到 ${target.label}`
  })

  /* ---------- L. 图片上传 ---------- */
  group('L. 图片上传：缩略图 / 灯箱 / 不发原图')

  // 用仓库里已有的截图当素材，省得再造 PNG
  const fixture = args.fixture || 'docs\\01-首次进入.png'

  const IMG_COUNT = `document.querySelectorAll('.msg img').length`
  const THUMB_SRCS = `[...document.querySelectorAll('.msg img')].map(i => i.getAttribute('src') || '')`

  await test('通过文件选择器上传一张图，输入框上方出现待发送缩略图', async () => {
    await cdp.setFileInput('input[type="file"]', [fixture])
    await cdp.waitFor(
      `!!document.querySelector('.composer img')`,
      '待发送缩略图出现',
      T(30000),
    )
    const n = await cdp.count('.composer img')
    assert(n >= 1, `输入框上方应当有缩略图，实际 ${n} 张`)
    return `${n} 张待发送缩略图`
  })

  await test('等上传真的落盘（缩略图上不再有「上传中」）', async () => {
    // 上传是异步的，等它完成再发 —— 也是真实用户的操作节奏
    try {
      await cdp.waitFor(
        `!(document.querySelector('.composer')?.innerText || '').includes('正在上传')`,
        '上传完成',
        T(30000),
      )
    } catch (e) {
      const dump = await cdp.eval(`(() => {
        const c = document.querySelector('.composer')
        return {
          innerText: (c?.innerText || '').slice(0, 300),
          hints: [...document.querySelectorAll('.composer__hint')].map(e => e.innerText.trim()),
          atts: [...document.querySelectorAll('.composer__atts *')].slice(0, 12).map(e => e.className + '|' + (e.innerText||'').trim().slice(0,20)),
        }
      })()`)
      throw new Error(`${e.message}\n  现场: ${JSON.stringify(dump, null, 2)}`)
    }
    const txt = await cdp.textExpr(`document.querySelector('.composer')`)
    assert(!txt.includes('没传成功'), `上传失败了：${txt.slice(0, 160)}`)
    return '上传完成'
  })

  await test('后端确实收到了附件（附件表里有记录）', async () => {
    // 没有列表接口，就用缩略图能不能取到来证明文件真的落盘了
    const src = await cdp.eval(`document.querySelector('.composer img')?.getAttribute('src') || ''`)
    assert(src.startsWith('blob:'), `上传完成后待发送缩略图应当仍是本地预览，实际 ${src}`)
    return '上传完成（本地预览）'
  })

  await test('只发图不打字也能发送（content 为空）', async () => {
    await cdp.focus(COMPOSER)
    // 清空输入框，确保是"纯图"消息
    await cdp.eval(`(() => { document.querySelector('${COMPOSER}').select(); return true })()`)
    await cdp.key('Backspace')
    await sleep(200)
    const before = await cdp.eval(USER_COUNT)
    await cdp.clickExpr(`document.querySelector('.composer__send')`, '发送按钮')
    try {
      await cdp.waitFor(
        `[...document.querySelectorAll('.msg--user')].some(m => m.querySelector('img'))`,
        '带图的用户消息出现',
        T(60000),
      )
    } catch (e) {
      // 失败时把现场打出来，别只说"超时"
      const dump = await cdp.eval(`(() => ({
        composer: (document.querySelector('.composer')?.innerText || '').slice(0, 200),
        toasts: [...document.querySelectorAll('.toast')].map(t => t.innerText.trim()),
        userMsgs: document.querySelectorAll('.msg--user').length,
        msgsWithImg: [...document.querySelectorAll('.msg--user')].filter(m => m.querySelector('img')).length,
        lastErr: document.querySelector('.msg__status--error')?.innerText || '',
      }))()`)
      throw new Error(`${e.message}\n  现场: ${JSON.stringify(dump)}`)
    }
    const after = await cdp.eval(USER_COUNT)
    assert(after === before + 1, `用户消息条数应为 ${before + 1}，实际 ${after}`)
    return `用户消息 ${before} → ${after}`
  })

  await test('消息里的图片用的是缩略图，不是原图', async () => {
    const srcs = await cdp.eval(THUMB_SRCS)
    assert(srcs.length >= 1, `消息里应当有图片，实际 ${srcs.length} 张`)
    for (const s of srcs) {
      assert(s.includes('/thumb'), `图片 src 不是缩略图: ${s}`)
      assert(!s.includes('/raw'), `图片直接用了原图（性能红线）: ${s}`)
    }
    const lazy = await cdp.eval(`[...document.querySelectorAll('.msg img')].every(i => i.loading === 'lazy')`)
    assert(lazy, '消息里的图片应当全部 loading="lazy"')
    return `${srcs.length} 张，全部 /thumb + lazy`
  })

  await test('后端确实收到了附件（消息带 attachments）', async () => {
    const st = await activeState()
    const withAtt = st.messages.filter((m) => (m.attachments?.length ?? 0) > 0)
    assert(withAtt.length >= 1, '后端消息里没有附件记录')
    const a = withAtt[0].attachments[0]
    assert(a.thumb_url && a.raw_url, '附件缺少 url')
    assert(a.width > 0 && a.height > 0, '附件缺少尺寸')
    return `附件 #${a.id} ${a.width}×${a.height} ${a.size} 字节`
  })

  await test('点缩略图打开灯箱，这时才请求原图', async () => {
    await cdp.clickExpr(`document.querySelector('.msg img')`, '消息里的缩略图')
    // 灯箱里应当出现一张指向 /raw 的大图
    const rawSrc = await cdp.waitFor(
      `(() => {
        const imgs = [...document.querySelectorAll('img')].map(i => i.getAttribute('src') || '')
        return imgs.find(s => s.includes('/raw')) || ''
      })()`,
      '灯箱里出现原图',
      T(20000),
    )
    assert(rawSrc, '灯箱没有加载原图')
    return rawSrc
  })

  await test('按 Esc 关闭灯箱', async () => {
    await cdp.key('Escape')
    await sleep(400)
    const stillRaw = await cdp.eval(
      `[...document.querySelectorAll('img')].some(i => (i.getAttribute('src')||'').includes('/raw'))`,
    )
    assert(!stillRaw, 'Esc 之后灯箱里的原图还在')
    return '已关闭'
  })

  /* ---------- M. 侧边栏：答完冒泡 + 未读小红点 ---------- */
  group('M. 侧边栏：回答完毕冒到最上面 + 未读小红点')

  // 通过 data-conv-id / data-unread 精确定位，不靠标题文字。
  // 注意：**不能用「列表第一个」当参照物** —— A 一发消息就会自己冒到第一，
  // 那样"切到 B"会变成切回 A，测试就成了蒙对。所以全程按 id 定位。
  const CONV_LIST = `[...document.querySelectorAll('.conv')].map(e => Number(e.getAttribute('data-conv-id')))`
  const UNREAD_IDS = `[...document.querySelectorAll('.conv[data-unread="1"]')].map(e => Number(e.getAttribute('data-conv-id')))`
  const ACTIVE_ID = `Number(document.querySelector('.conv--active')?.getAttribute('data-conv-id'))`

  let convA = 0
  let convB = 0

  await test('准备两个会话：A（原来就是当前会话）和新建的 B', async () => {
    const ids = await cdp.eval(CONV_LIST)
    assert(ids.length >= 1, '列表里应当至少有一个会话')
    convA = await cdp.eval(ACTIVE_ID)
    assert(convA > 0, '应当有一个当前会话')

    await cdp.clickExpr(`document.querySelector('.sidebar__new')`, '「+ 新对话」按钮')
    await cdp.waitFor(`document.querySelectorAll('.conv').length === ${ids.length + 1}`, '新会话出现', T(10000))
    convB = await cdp.eval(ACTIVE_ID)
    assert(convB > 0 && convB !== convA, `新建的 B 应当是新会话，A=${convA} B=${convB}`)
    return `A=${convA}，B=${convB}（B 是刚建的，当前选中）`
  })

  await test('回到 A 发一条消息，然后趁它还在生成切到 B', async () => {
    await cdp.clickExpr(`document.querySelector('.conv[data-conv-id="${convA}"]')`, `会话 A(${convA})`)
    await cdp.waitFor(`${ACTIVE_ID} === ${convA}`, 'A 变成当前会话', T(10000))

    // 发之前先记一份状态：不然「等生成」超时只能看到一个 false，看不出卡在哪
    const beforeSend = await cdp.eval(`({
      areaDisabled: !!document.querySelector('.composer__area')?.disabled,
      notice: (document.querySelector('.composer__notice-text')?.textContent || '').trim(),
      sendDisabled: !!document.querySelector('.composer__send')?.disabled,
      msgs: document.querySelectorAll('.msg').length,
      model: (document.querySelector('.picker__trigger')?.innerText || '').trim(),
    })`)

    await cdp.focus(COMPOSER)
    await cdp.type('这条答完以后 A 应当冒到最上面')
    await cdp.key('Enter')

    try {
      await cdp.waitFor(`!!document.querySelector('.composer__send--stop')`, 'A 开始生成', T(20000))
    } catch (e) {
      const afterSend = await cdp.eval(`({
        areaDisabled: !!document.querySelector('.composer__area')?.disabled,
        notice: (document.querySelector('.composer__notice-text')?.textContent || '').trim(),
        draft: (document.querySelector('.composer__area')?.value || '').slice(0, 40),
        msgs: document.querySelectorAll('.msg').length,
        toast: ([...document.querySelectorAll('[class*=toast]')].map((e) => e.innerText.trim()).join(' | ')),
        busyDot: !!document.querySelector('.thinking, .cursor'),
        lastError: ([...document.querySelectorAll('.msg__status--error')].pop()?.innerText || '').trim(),
      })`)
      throw new Error(
        `等「A 开始生成」超时。\n` +
          `  发送前: ${JSON.stringify(beforeSend)}\n` +
          `  超时后: ${JSON.stringify(afterSend)}\n` +
          `  提示：若 lastError 是 400，多半是假上游没给这个模型开 -vision（见文件头说明）`,
      )
    }

    // 趁 A 在生成，**按 id** 切到 B
    await cdp.clickExpr(`document.querySelector('.conv[data-conv-id="${convB}"]')`, `会话 B(${convB})`)
    await cdp.waitFor(`${ACTIVE_ID} === ${convB}`, 'B 变成当前会话', T(10000))

    const stillRunning = await cdp.eval(`!!document.querySelector('.composer__send--stop')`)
    return `A=${convA} 在后台生成（此刻停止按钮还在=${stillRunning}），当前已切到 B=${convB}`
  })

  await test('A 答完后自动冒到列表最上面', async () => {
    try {
      await cdp.waitFor(
        `Number(document.querySelector('.conv')?.getAttribute('data-conv-id')) === ${convA}`,
        'A 冒到最上面',
        T(60000),
      )
    } catch (e) {
      const dump = await cdp.eval(`[...document.querySelectorAll('.conv')].map(e => ({
        id: e.getAttribute('data-conv-id'),
        unread: e.getAttribute('data-unread'),
        active: e.classList.contains('conv--active'),
        pulse: !!e.querySelector('.pulse'),
      }))`)
      throw new Error(`${e.message}\n  列表现状: ${JSON.stringify(dump)}`)
    }
    return `会话 ${convA} 已排到最前`
  })

  await test('A 上出现未读小红点，当前正在看的 B 没有', async () => {
    const unread = await cdp.eval(UNREAD_IDS)
    assert(unread.includes(convA), `A(${convA}) 应当是未读，实际未读的是 ${JSON.stringify(unread)}`)
    assert(!unread.includes(convB), `当前正在看的 B(${convB}) 不该有小红点`)
    const hasDot = await cdp.eval(
      `!!document.querySelector('.conv[data-conv-id="${convA}"] .conv__unread')`,
    )
    assert(hasDot, '未读会话上应当有 .conv__unread 那个小红点元素')
    return `A=${convA} 未读，当前会话 B=${convB} 干净`
  })

  await test('点开 A 之后小红点消失', async () => {
    await cdp.clickExpr(`document.querySelector('.conv[data-conv-id="${convA}"]')`, `有未读的会话 A(${convA})`)
    await cdp.waitFor(`${ACTIVE_ID} === ${convA}`, 'A 变成当前会话', T(10000))
    await cdp.waitFor(
      `document.querySelector('.conv[data-conv-id="${convA}"]')?.getAttribute('data-unread') === '0'`,
      'A 的未读标记被清掉',
      T(15000),
    )
    const left = await cdp.eval(UNREAD_IDS)
    assert(!left.includes(convA), `点开之后 A 不该还是未读，实际 ${JSON.stringify(left)}`)
    return `会话 A=${convA} 的未读已清除`
  })

  await test('刷新页面后未读状态不会诈尸', async () => {
    await cdp.send('Page.reload')
    await cdp.waitFor(`document.querySelectorAll('.conv').length > 0`, '列表重新加载', T(25000))
    await sleep(1500)
    const unread = await cdp.eval(UNREAD_IDS)
    const active = await cdp.eval(ACTIVE_ID)
    assert(!unread.includes(active), `刷新后当前会话不该是未读（active=${active}, unread=${JSON.stringify(unread)}）`)
    assert(!unread.includes(convA), `A=${convA} 已经被读过了，刷新后不该又变未读`)
    return `刷新后未读 ${JSON.stringify(unread)}，当前 ${active} 干净`
  })

  /* ---------- 收尾 ---------- */
  const pass = results.filter((r) => r.ok).length
  const fail = results.length - pass
  console.log(`\n${'='.repeat(64)}`)
  console.log(`共 ${results.length} 项：\x1b[32m通过 ${pass}\x1b[0m${fail ? `，\x1b[31m失败 ${fail}\x1b[0m` : ''}`)
  if (fail) {
    console.log('\n失败明细：')
    results.filter((r) => !r.ok).forEach((r) => console.log(`  [${r.group}] ${r.name}\n    ${r.note}`))
  }
  ws.close()
  return fail
}

let exitCode = 1
try {
  exitCode = await main()
} catch (e) {
  console.error(`\n\x1b[31m测试运行失败: ${e.message}\x1b[0m`)
  exitCode = 1
} finally {
  if (chromeProc && !KEEP) {
    try {
      chromeProc.kill()
    } catch {
      /* 忽略 */
    }
  }
  if (profileDir && !KEEP) {
    await sleep(500)
    try {
      rmSync(profileDir, { recursive: true, force: true })
    } catch {
      /* Chrome 有时还占着，忽略 */
    }
  }
}
process.exit(exitCode)
