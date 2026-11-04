import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const token = process.env.MEMORY_TEST_TOKEN
const base = process.env.MEMORY_TEST_BASE || 'https://dmbot.nat100.top'
const sourceID = Number(process.env.MEMORY_TEST_SOURCE)
const marker = process.env.MEMORY_TEST_MARKER || 'AKATOSH-MEMORY-60427'
const editMarker = 'AKATOSH-MEMORY-EDIT-20261104'
const cancelMarker = 'AKATOSH-MEMORY-CANCEL-20261104'
if (!token) throw new Error('MEMORY_TEST_TOKEN is required')
if (!sourceID) throw new Error('MEMORY_TEST_SOURCE is required')

const profile = mkdtempSync(join(tmpdir(), 'memory-edit-chromium-'))
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
async function openArchive() {
  await waitFor(`!!document.querySelector('.sidebar__memory')`)
  await evaluate(`document.querySelector('.sidebar__memory').click()`)
  await waitFor(`!!document.querySelector('.memory-dialog')`)
  await waitFor(`[...document.querySelectorAll('.memory-list__item')].some(el => el.textContent.includes('Session #${sourceID}'))`)
  await evaluate(`[...document.querySelectorAll('.memory-list__item')].find(el => el.textContent.includes('Session #${sourceID}')).click()`)
  await waitFor(`document.querySelector('.memory-detail__content')?.textContent.includes('${marker}')`)
}
async function replaceDraft(suffix) {
  await evaluate(`(() => {
    const el = document.querySelector('.memory-detail__textarea');
    el.value += ${JSON.stringify(suffix)};
    el.dispatchEvent(new Event('input', { bubbles: true }));
  })()`)
}

try {
  const target = await send('Target.createTarget', { url: 'about:blank' })
  const attached = await send('Target.attachToTarget', { targetId: target.result.targetId, flatten: true })
  sessionId = attached.result.sessionId
  await send('Page.enable')
  await send('Runtime.enable')
  await send('Page.navigate', { url: `${base}/?token=${token}` })
  await openArchive()
  await evaluate(`document.querySelector('.memory-detail__edit').click()`)
  await waitFor(`document.querySelector('.memory-detail__textarea')?.value.includes('${marker}')`)
  await replaceDraft(`\n${editMarker}\n`)
  const editingScreenshot = await send('Page.captureScreenshot', { format: 'png' })
  writeFileSync('/tmp/akatosh-memory-editing.png', Buffer.from(editingScreenshot.result.data, 'base64'))
  await evaluate(`document.querySelector('.memory-detail__edit-actions .btn--primary').click()`)
  await waitFor(`document.querySelector('.memory-detail__content')?.textContent.includes('${editMarker}')`)
  const saved = await evaluate(`fetch('/api/memory/archives/${sourceID}').then(r => r.json())`)
  if (!saved.content?.includes(editMarker)) throw new Error('Saved marker missing from API')

  await send('Page.reload', { ignoreCache: true })
  await openArchive()
  await waitFor(`document.querySelector('.memory-detail__content')?.textContent.includes('${editMarker}')`)
  const screenshot = await send('Page.captureScreenshot', { format: 'png' })
  writeFileSync('/tmp/akatosh-memory-edited.png', Buffer.from(screenshot.result.data, 'base64'))

  await evaluate(`document.querySelector('.memory-detail__edit').click()`)
  await waitFor(`!!document.querySelector('.memory-detail__textarea')`)
  await replaceDraft(`\n${cancelMarker}\n`)
  await evaluate(`window.__confirmCalls = 0; window.confirm = () => { window.__confirmCalls++; return false }`)
  await evaluate(`document.querySelector('.memory-dialog .modal__head button[title="关闭"]').click()`)
  await sleep(300)
  const guard = await evaluate(`({ open: !!document.querySelector('.memory-dialog'), calls: window.__confirmCalls })`)
  if (!guard.open || guard.calls !== 1) throw new Error(`Unsaved close guard failed: ${JSON.stringify(guard)}`)
  await evaluate(`document.querySelector('.memory-detail__edit-actions .btn:not(.btn--primary)').click()`)
  await waitFor(`!!document.querySelector('.memory-detail__content')`)
  const afterCancel = await evaluate(`fetch('/api/memory/archives/${sourceID}').then(r => r.json())`)
  if (afterCancel.content?.includes(cancelMarker)) throw new Error('Cancelled edit was persisted')

  console.log(JSON.stringify({
    sourceID,
    saved: saved.content.includes(editMarker),
    persistedAfterReload: true,
    cancelDiscarded: !afterCancel.content.includes(cancelMarker),
    unsavedCloseGuard: guard,
    totalChars: afterCancel.total_chars,
    editingScreenshot: '/tmp/akatosh-memory-editing.png',
    screenshot: '/tmp/akatosh-memory-edited.png',
  }, null, 2))
} finally {
  proc.stdio[3].end()
  proc.kill()
  await sleep(300)
  rmSync(profile, { recursive: true, force: true })
}
