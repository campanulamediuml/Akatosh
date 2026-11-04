import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const token = process.env.MEMORY_TEST_TOKEN
const base = process.env.MEMORY_TEST_BASE || 'https://dmbot.nat100.top'
const sourceID = Number(process.env.MEMORY_TEST_SOURCE || 213)
const marker = process.env.MEMORY_TEST_MARKER || 'AKATOSH-MEMORY-60427'
if (!token) throw new Error('MEMORY_TEST_TOKEN is required')
const profile = mkdtempSync(join(tmpdir(), 'memory-read-chromium-'))
const proc = spawn('/home/campanula/bin/chromium', [
  '--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--hide-scrollbars',
  '--window-size=1440,900', `--user-data-dir=${profile}`, '--remote-debugging-pipe', 'about:blank',
], { stdio: ['ignore', 'ignore', 'ignore', 'pipe', 'pipe'] })
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
let seq = 0
let incoming = ''
let sessionId = ''
const waiting = new Map()
proc.stdio[4].on('data', (chunk) => {
  incoming += chunk.toString()
  let end = incoming.indexOf('\0')
  while (end >= 0) {
    const msg = JSON.parse(incoming.slice(0, end))
    incoming = incoming.slice(end + 1)
    if (msg.id && waiting.has(msg.id)) {
      waiting.get(msg.id)(msg)
      waiting.delete(msg.id)
    }
    end = incoming.indexOf('\0')
  }
})
const send = (method, params = {}) => new Promise((resolve) => {
  const id = ++seq
  waiting.set(id, resolve)
  const message = { id, method, params }
  if (sessionId) message.sessionId = sessionId
  proc.stdio[3].write(JSON.stringify(message) + '\0')
})
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
  const target = await send('Target.createTarget', { url: 'about:blank' })
  const attached = await send('Target.attachToTarget', { targetId: target.result.targetId, flatten: true })
  sessionId = attached.result.sessionId
  await send('Page.enable')
  await send('Runtime.enable')
  await send('Page.navigate', { url: `${base}/?token=${token}` })
  await waitFor(`!!document.querySelector('.memory-switch')`)
  const previousID = await evaluate(`Number(document.querySelector('.conv--active')?.dataset.convId || 0)`)
  await evaluate(`document.querySelector('.sidebar__new').click()`)
  await waitFor(`Number(document.querySelector('.conv--active')?.dataset.convId || 0) !== ${previousID}`)
  await waitFor(`!!document.querySelector('.memory-switch')`)
  const convID = await evaluate(`Number(document.querySelector('.conv--active')?.dataset.convId)`)
  await evaluate(`document.querySelector('.memory-switch').click()`)
  await waitFor(`document.querySelector('.memory-switch')?.getAttribute('aria-checked') === 'true'`)
  await evaluate(`(() => { const el = document.querySelector('.composer__area'); el.value = '请调用 memory_read_session 工具读取 Session #${sourceID} 的详细归档，然后告诉我其中的测试暗号。必须实际调用工具，不要猜。'; el.dispatchEvent(new Event('input', { bubbles: true })); })()`)
  await evaluate(`document.querySelector('.composer__send').click()`)
  await waitFor(`document.querySelector('.composer__area')?.value === ''`)
  await waitFor(`!document.querySelector('.composer__send--stop')`, 120000)
  await sleep(1500)
  const state = await evaluate(`fetch('/api/conversations/${convID}/state').then(r => r.json())`)
  const assistant = state.messages?.filter((m) => m.role === 'assistant').at(-1)
  if (!assistant?.content?.includes(marker)) throw new Error(`Answer does not contain marker: ${JSON.stringify(assistant)}`)
  if (!assistant?.search_log?.includes('memory')) throw new Error(`No memory tool record: ${JSON.stringify(assistant)}`)
  const screenshot = await send('Page.captureScreenshot', { format: 'png' })
  writeFileSync('/tmp/akatosh-memory-read.png', Buffer.from(screenshot.result.data, 'base64'))
  writeFileSync('/tmp/akatosh-memory-read-result.json', JSON.stringify({ convID, sourceID, assistant }, null, 2))
  console.log(JSON.stringify({ convID, sourceID, answer: assistant.content, search_log: assistant.search_log, screenshot: '/tmp/akatosh-memory-read.png' }, null, 2))
} finally {
  proc.stdio[3].end()
  proc.kill()
  await sleep(300)
  rmSync(profile, { recursive: true, force: true })
}
