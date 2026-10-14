/**
 * 用 CDP 驱动无头 Chrome 抓真实渲染截图，验证流式链路。
 *
 *   node tools/shoot.mjs <输出目录>
 *
 * 前提：
 *   1. 后端跑在 127.0.0.1:28888，假上游跑在 127.0.0.1:28899 且每片间隔较大
 *   2. Chrome 以 --headless --remote-debugging-port=9222 启动
 *
 * 抓三张：空闲态 / 生成途中 / 生成途中刷新页面（验证 CoT 能续上）
 */
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'

const BASE = 'http://127.0.0.1:28888'
const CDP = 'http://127.0.0.1:9222'
const outDir = process.argv[2] || '.'

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function findTarget() {
  for (let i = 0; i < 40; i++) {
    try {
      const list = await (await fetch(CDP + '/json/list')).json()
      const t = list.find((x) => x.type === 'page')
      if (t) return t
    } catch {
      /* Chrome 还没起来 */
    }
    await sleep(250)
  }
  throw new Error('找不到 Chrome 的 page target')
}

async function main() {
  const target = await findTarget()
  const ws = new WebSocket(target.webSocketDebuggerUrl)
  await new Promise((res, rej) => {
    ws.onopen = res
    ws.onerror = () => rej(new Error('CDP 连接失败'))
  })

  let seq = 0
  const waiting = new Map()
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data)
    if (msg.id && waiting.has(msg.id)) {
      waiting.get(msg.id)(msg)
      waiting.delete(msg.id)
    }
  }
  const send = (method, params = {}) =>
    new Promise((resolve) => {
      const id = ++seq
      waiting.set(id, resolve)
      ws.send(JSON.stringify({ id, method, params }))
    })

  await send('Page.enable')
  await send('Runtime.enable')

  async function shot(name) {
    const r = await send('Page.captureScreenshot', { format: 'png' })
    const file = join(outDir, name)
    writeFileSync(file, Buffer.from(r.result.data, 'base64'))
    console.log('  截图:', file)
  }

  /** 在页面里执行一段 JS，把结果打回来（用于确认页面内部状态） */
  async function evaluate(expr) {
    const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true })
    return r.result?.result?.value
  }

  /** 量「最后一条」消息的 CoT 文本长度：querySelector 拿到的是第一条（可能早已生成完） */
  const COT_LEN = `(() => {
    const els = [...document.querySelectorAll('.reasoning')]
    return els.length ? els[els.length - 1].innerText.length : -1
  })()`

  console.log('[1] 打开页面，等状态加载完...')
  await send('Page.navigate', { url: BASE })
  await sleep(3500)
  console.log('    当前会话:', await evaluate('document.querySelector(".topbar__name")?.textContent'))

  const convs = await (await fetch(BASE + '/api/conversations')).json()
  const conv = convs[0]
  console.log(`[2] 在会话 ${conv.id}「${conv.title}」发起一轮生成（途中截图）...`)
  await fetch(`${BASE}/api/conversations/${conv.id}/messages`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ content: '再讲一遍，这次讲慢一点，我要完整看到思考过程。' }),
  })
  await sleep(2000)
  const midCot = await evaluate(COT_LEN)
  await shot('shot-streaming.png')
  console.log('    生成途中：最后一条消息的 CoT 文本长度 =', midCot)

  console.log('[3] 生成途中刷新页面（验证 CoT 能续上、不从头开始）...')
  await send('Page.reload')
  await sleep(1800)
  const afterCot = await evaluate(COT_LEN)
  await shot('shot-resume.png')
  console.log('    刷新之后：最后一条消息的 CoT 文本长度 =', afterCot)
  console.log(
    afterCot > midCot
      ? `    [PASS] 刷新后 CoT 变长了（${midCot} → ${afterCot}），说明是续上的、没有从头开始`
      : `    [FAIL] 刷新后 CoT 没有继续增长（${midCot} → ${afterCot}）`,
  )

  console.log('[4] 等生成结束，然后验证「编辑正文」...')
  for (let i = 0; i < 40; i++) {
    const st = await (await fetch(`${BASE}/api/conversations/${conv.id}/state`)).json()
    if (!st.generating) break
    await sleep(500)
  }
  await sleep(800)
  const clicked = await evaluate(`(() => {
    const btns = [...document.querySelectorAll('button')].filter(b =>
      (b.title || '').includes('编辑') || (b.textContent || '').trim() === '编辑')
    if (!btns.length) return 'no-edit-button'
    btns[btns.length - 1].click()
    return 'clicked:' + btns.length
  })()`)
  console.log('    点击编辑:', clicked)
  await sleep(700)
  const hasTextarea = await evaluate(`!!document.querySelector('textarea.msg__editor, textarea[data-edit], .msg--editing textarea')`)
  console.log('    出现编辑框:', hasTextarea)
  await shot('shot-editing.png')

  ws.close()
  return { midCot, afterCot, clicked, hasTextarea }
}

main()
  .then((r) => {
    console.log('完成:', JSON.stringify(r))
  })
  .catch((e) => {
    console.error('失败:', e.message)
    process.exit(1)
  })
