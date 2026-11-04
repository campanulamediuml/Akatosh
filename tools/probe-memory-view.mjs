import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const token = process.env.MEMORY_TEST_TOKEN
const sourceID = Number(process.env.MEMORY_TEST_SOURCE || 221)
const marker = process.env.MEMORY_TEST_MARKER || 'AKATOSH-MEMORY-60427'
if (!token) throw new Error('MEMORY_TEST_TOKEN is required')
const profile = mkdtempSync(join(tmpdir(), 'memory-view-chromium-'))
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
  await send('Page.navigate', { url: `https://dmbot.nat100.top/?token=${token}` })
  await waitFor(`!!document.querySelector('.sidebar__memory')`)
  await evaluate(`document.querySelector('.sidebar__memory').click()`)
  await waitFor(`!!document.querySelector('.memory-dialog')`)
  await waitFor(`document.querySelectorAll('.memory-list__item').length > 0`)
  await evaluate(`[...document.querySelectorAll('.memory-list__item')].find(el => el.textContent.includes('Session #${sourceID}')).click()`)
  await waitFor(`document.querySelector('.memory-detail__content')?.textContent.includes('${marker}')`)
  const result = await evaluate(`({
    title: document.querySelector('.memory-list__item--active')?.textContent,
    detail: document.querySelector('.memory-detail__content')?.textContent,
    count: document.querySelectorAll('.memory-list__item').length,
  })`)
  const screenshot = await send('Page.captureScreenshot', { format: 'png' })
  writeFileSync('/tmp/akatosh-memory-view.png', Buffer.from(screenshot.result.data, 'base64'))
  console.log(JSON.stringify({ count: result.count, title: result.title, detailLength: result.detail.length, markerFound: result.detail.includes(marker), screenshot: '/tmp/akatosh-memory-view.png' }, null, 2))
} finally {
  proc.stdio[3].end()
  proc.kill()
  await sleep(300)
  rmSync(profile, { recursive: true, force: true })
}
