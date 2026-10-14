/**
 * 对照实验：Chrome 的 textarea 里，Ctrl+Enter 到底会不会插入换行？
 *
 * 方法：往页面里插一个**裸 textarea**（没有任何 JS 处理器），
 * 用 CDP 派发和真实按键完全一致的 keyDown（带 text:'\r'），
 * 看浏览器默认行为有没有插入换行。这样就把「我的派发方式」和
 * 「浏览器真实行为」分开了。
 */
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const PORT = Number(process.argv[2] || 9333)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const CHROME = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
].find((p) => existsSync(p))

const profile = mkdtempSync(join(tmpdir(), 'probe-chrome-'))
const proc = spawn(
  CHROME,
  [
    '--headless=new',
    '--disable-gpu',
    '--no-sandbox',
    '--no-first-run',
    `--user-data-dir=${profile}`,
    `--remote-debugging-port=${PORT}`,
    'about:blank',
  ],
  { stdio: 'ignore' },
)

async function endpoint() {
  for (let i = 0; i < 60; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()
      const t = list.find((x) => x.type === 'page')
      if (t) return t.webSocketDebuggerUrl
    } catch {
      /* 等 */
    }
    await sleep(300)
  }
  throw new Error('Chrome 起不来')
}

const ws = new WebSocket(await endpoint())
await new Promise((res, rej) => {
  ws.onopen = res
  ws.onerror = () => rej(new Error('ws 连接失败'))
})

let seq = 0
const waiting = new Map()
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data)
  if (m.id && waiting.has(m.id)) {
    waiting.get(m.id)(m)
    waiting.delete(m.id)
  }
}
const send = (method, params = {}) =>
  new Promise((resolve) => {
    const id = ++seq
    waiting.set(id, resolve)
    ws.send(JSON.stringify({ id, method, params }))
  })
const evaluate = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  if (r.result?.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails))
  return r.result.result.value
}

await send('Page.enable')
await send('Runtime.enable')
await send('Page.navigate', { url: 'data:text/html,<textarea id=t rows=5 cols=40></textarea>' })
await sleep(800)

/** 派发一次 Enter；withText 决定要不要带 text:'\r'（Chrome 为「会产生文本」的按键才会带） */
async function pressEnter({ ctrl = false, shift = false, withText }) {
  let modifiers = 0
  if (ctrl) modifiers |= 2
  if (shift) modifiers |= 8
  const base = { modifiers, windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13, code: 'Enter', key: 'Enter' }
  const type = withText ? 'keyDown' : 'rawKeyDown'
  const extra = withText ? { text: '\r', unmodifiedText: '\r' } : {}
  await send('Input.dispatchKeyEvent', { type, ...base, ...extra })
  await send('Input.dispatchKeyEvent', { type: 'keyUp', ...base })
  await sleep(150)
}

async function probe(label, opts) {
  await evaluate(`(() => { const t = document.querySelector('#t'); t.value='AB'; t.focus(); t.setSelectionRange(2,2); return true })()`)
  await pressEnter(opts)
  const v = await evaluate(`document.querySelector('#t').value`)
  const inserted = v.includes('\n')
  console.log(
    `  ${label.padEnd(34)} textarea = ${JSON.stringify(v).padEnd(14)} → ${inserted ? '\x1b[32m插入了换行\x1b[0m' : '\x1b[31m没有换行\x1b[0m'}`,
  )
  return inserted
}

console.log('\n裸 textarea（无任何 JS 拦截）里各组合键的浏览器默认行为：')
const plain = await probe('Enter', { withText: true })
const shift = await probe('Shift+Enter', { withText: true, shift: true })
const ctrlWithText = await probe('Ctrl+Enter（带 text）', { withText: true, ctrl: true })
const ctrlRaw = await probe('Ctrl+Enter（不带 text）', { withText: false, ctrl: true })

console.log('\n结论：')
if (plain && shift && !ctrlWithText && !ctrlRaw) {
  console.log('  \x1b[33mChrome 不会为 Ctrl+Enter 插入换行（带不带 text 都不会）。\x1b[0m')
  console.log('  → 依赖「不拦截、交给浏览器默认行为」的 Ctrl+Enter 换行 \x1b[31m是坏的\x1b[0m，必须在 JS 里显式插入 \\n。')
} else if (ctrlWithText) {
  console.log('  Ctrl+Enter 带 text 时浏览器会插入换行 → 之前测试里没换行是我的派发方式不对，应用没问题。')
} else {
  console.log('  结果不如预期，需要人工判断。')
}

ws.close()
proc.kill()
await sleep(300)
try {
  rmSync(profile, { recursive: true, force: true })
} catch {
  /* 忽略 */
}
process.exit(0)
