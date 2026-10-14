/**
 * 抓多供应商界面的截图：设置弹窗（供应商列表）+ 分组后的模型选择器。
 *
 *   node tools/shoot-multi.mjs <输出目录>
 */
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, existsSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const BASE = process.argv[2] || 'http://127.0.0.1:28888'
const OUT = process.argv[3] || '.'
const PORT = 9555
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const CHROME = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
].find((p) => existsSync(p))

const profile = mkdtempSync(join(tmpdir(), 'shoot-multi-'))
const proc = spawn(
  CHROME,
  ['--headless=new', '--disable-gpu', '--no-sandbox', '--hide-scrollbars', '--no-first-run',
   `--user-data-dir=${profile}`, `--remote-debugging-port=${PORT}`, '--window-size=1440,1100', 'about:blank'],
  { stdio: 'ignore' },
)

async function endpoint() {
  for (let i = 0; i < 60; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()
      const t = list.find((x) => x.type === 'page')
      if (t) return t.webSocketDebuggerUrl
    } catch { /* 等 */ }
    await sleep(300)
  }
  throw new Error('Chrome 起不来')
}

const ws = new WebSocket(await endpoint())
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = () => rej(new Error('ws')) })
let seq = 0
const waiting = new Map()
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data)
  if (m.id && waiting.has(m.id)) { waiting.get(m.id)(m); waiting.delete(m.id) }
}
const send = (method, params = {}) => new Promise((resolve) => {
  const id = ++seq; waiting.set(id, resolve); ws.send(JSON.stringify({ id, method, params }))
})
const evaluate = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  if (r.result?.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails).slice(0, 300))
  return r.result.result.value
}
async function shot(name) {
  const r = await send('Page.captureScreenshot', { format: 'png' })
  const file = join(OUT, name)
  writeFileSync(file, Buffer.from(r.result.data, 'base64'))
  console.log('  截图:', file)
}

await send('Page.enable')
await send('Runtime.enable')
await send('Page.navigate', { url: BASE })
await sleep(4000)

console.log('[1] 打开设置弹窗（供应商列表）')
await evaluate(`document.querySelector('.topbar [title="设置"]').click(); true`)
await sleep(1200)
await shot('docs-多供应商-设置.png')

console.log('[2] 打开模型选择器（按供应商分组）')
await evaluate(`document.querySelector('.modal .icon-btn[title="关闭"]')?.click(); true`)
await sleep(800)
await evaluate(`document.querySelector('.picker__trigger').click(); true`)
await sleep(800)
const info = await evaluate(`(() => ({
  groups: [...document.querySelectorAll('.picker__menu .picker__group')].map(e => e.innerText.trim()),
  items: [...document.querySelectorAll('.picker__menu .picker__item')].map(e => e.innerText.trim().split('\\n')[0]),
}))()`)
console.log('  分组:', JSON.stringify(info.groups))
console.log('  模型:', JSON.stringify(info.items))
await shot('docs-多供应商-模型分组.png')

ws.close()
proc.kill()
await sleep(300)
try { rmSync(profile, { recursive: true, force: true }) } catch { /* 忽略 */ }
process.exit(0)
