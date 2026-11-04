import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const token = process.env.MEMORY_TEST_TOKEN
const base = process.env.MEMORY_TEST_BASE || 'https://dmbot.nat100.top'
const chromium = process.env.CHROMIUM || '/home/campanula/bin/chromium'
if (!token) throw new Error('MEMORY_TEST_TOKEN is required')

const profile = mkdtempSync(join(tmpdir(), 'memory-chromium-'))
const proc = spawn(chromium, [
  '--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--hide-scrollbars',
  '--window-size=1440,900', `--user-data-dir=${profile}`, '--remote-debugging-pipe', 'about:blank',
], { stdio: ['ignore', 'ignore', 'ignore', 'pipe', 'pipe'] })
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

let seq = 0
let incoming = ''
const waiting = new Map()
proc.stdio[4].on('data', (chunk) => {
  incoming += chunk.toString()
  let end = incoming.indexOf('\0')
  while (end >= 0) {
    const msg = JSON.parse(incoming.slice(0, end))
    incoming = incoming.slice(end + 1)
    if (msg.error) console.error('CDP error:', JSON.stringify(msg))
    if (msg.id && waiting.has(msg.id)) {
      waiting.get(msg.id)(msg)
      waiting.delete(msg.id)
    }
    end = incoming.indexOf('\0')
  }
})
let sessionId = ''
const send = (method, params = {}) => new Promise((resolve) => {
  const id = ++seq
  waiting.set(id, resolve)
  const message = { id, method, params }
  if (sessionId) message.sessionId = sessionId
  proc.stdio[3].write(JSON.stringify(message) + '\0')
})
const target = await send('Target.createTarget', { url: 'about:blank' })
const attached = await send('Target.attachToTarget', { targetId: target.result.targetId, flatten: true })
sessionId = attached.result.sessionId

async function evaluate(expression) {
  const result = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  if (result.result?.exceptionDetails) throw new Error(JSON.stringify(result.result.exceptionDetails))
  return result.result?.result?.value
}
async function waitFor(expression, timeout = 30000) {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    if (await evaluate(expression)) return true
    await sleep(250)
  }
  throw new Error(`Timeout: ${expression}`)
}

try {
  await send('Page.enable')
  await send('Runtime.enable')
  await send('Page.navigate', { url: `${base}/?token=${token}` })
  await waitFor(`!!document.querySelector('.sidebar__new')`)
  await waitFor(`!!document.querySelector('.memory-switch')`)

  const previousID = await evaluate(`Number(document.querySelector('.conv--active')?.dataset.convId || 0)`)
  await evaluate(`document.querySelector('.sidebar__new').click()`)
  await waitFor(`Number(document.querySelector('.conv--active')?.dataset.convId || 0) !== ${previousID}`)
  await waitFor(`!!document.querySelector('.memory-switch')`)
  const convID = await evaluate(`Number(document.querySelector('.conv--active')?.dataset.convId)`)
  if (!convID) throw new Error('No active conversation')
  await evaluate(`document.querySelector('.memory-switch').click()`)
  await waitFor(`document.querySelector('.memory-switch')?.getAttribute('aria-checked') === 'true'`)

  await evaluate(`document.querySelector('button[title="设置"]').click()`)
  await waitFor(`!!document.querySelector('#background-provider')`)
  await evaluate(`(() => { const el = document.querySelector('#background-provider'); el.value = '1'; el.dispatchEvent(new Event('change', { bubbles: true })); })()`)
  await waitFor(`document.querySelector('#background-model')?.options.length > 1`)
  await waitFor(`document.querySelector('#background-model')?.disabled === false`)
  await evaluate(`(() => { const el = document.querySelector('#background-model'); el.value = 'deepseek-flash'; el.dispatchEvent(new Event('change', { bubbles: true })); })()`)
  await waitFor(`document.querySelector('#background-effort')?.value === 'low'`)
  await waitFor(`document.querySelector('#background-effort')?.disabled === false`)
  const savedBackground = await evaluate(`fetch('/api/memory/settings').then(r => r.json())`)
  if (savedBackground.background?.model_id !== 'deepseek-flash' || savedBackground.background?.effort !== 'low') {
    throw new Error(`Background model not saved: ${JSON.stringify(savedBackground)}`)
  }
  await evaluate(`document.querySelector('button[title="关闭"]').click()`)

  await send('Page.reload', { ignoreCache: true })
  await waitFor(`!!document.querySelector('[data-conv-id="${convID}"]')`)
  await evaluate(`document.querySelector('[data-conv-id="${convID}"]').click()`)
  await waitFor(`document.querySelector('.conv--active')?.dataset.convId === '${convID}'`)
  await waitFor(`document.querySelector('.memory-switch')?.getAttribute('aria-checked') === 'true'`)
  await evaluate(`document.querySelector('button[title="设置"]').click()`)
  await waitFor(`document.querySelector('#background-model')?.value === 'deepseek-flash'`)
  await evaluate(`document.querySelector('button[title="关闭"]').click()`)

  const marker = 'AKATOSH-MEMORY-60427'
  await evaluate(`(() => { const el = document.querySelector('.composer__area'); el.value = '这是长期记忆功能测试。请记住测试暗号 ${marker}，并简短回复已记住。'; el.dispatchEvent(new Event('input', { bubbles: true })); })()`)
  await evaluate(`document.querySelector('.composer__send').click()`)
  await waitFor(`document.querySelector('.composer__area')?.value === ''`)
  await waitFor(`!document.querySelector('.msg--streaming') && !document.querySelector('.composer__send--stop')`, 120000)
  await sleep(1500)
  const status = await evaluate(`fetch('/api/conversations/${convID}/memory').then(r => r.json())`)
  const messages = await evaluate(`fetch('/api/conversations/${convID}/state').then(r => r.json()).then(s => s.messages)`)
  const screenshot = await send('Page.captureScreenshot', { format: 'png' })
  writeFileSync('/tmp/akatosh-memory-ui.png', Buffer.from(screenshot.result.data, 'base64'))
  writeFileSync('/tmp/akatosh-memory-conversation.json', JSON.stringify({ convID, marker, status, messages }, null, 2))
  console.log(JSON.stringify({ convID, marker, memory: status, background: savedBackground.background, screenshot: '/tmp/akatosh-memory-ui.png' }, null, 2))
} finally {
  proc.stdio[3].end()
  proc.kill()
  await sleep(300)
  rmSync(profile, { recursive: true, force: true })
}
