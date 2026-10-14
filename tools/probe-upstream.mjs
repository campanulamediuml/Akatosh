/**
 * 探测真实上游：哪些模型可用、哪些会吐 reasoning_content、reasoning_effort 收不收。
 *
 *   node tools/probe-upstream.mjs <baseUrl> <apiKey> [model...]
 *
 * 只发很短的请求（max_tokens 不大），输出里不会打印 key。
 */

const base = (process.argv[2] || 'https://api.deepseek.com/v1').replace(/\/+$/, '')
const key = process.argv[3]
const models = process.argv.slice(4)

if (!key) {
  console.error('用法: node tools/probe-upstream.mjs <baseUrl> <apiKey> [model...]')
  process.exit(2)
}

const DEFAULT_MODELS = [
  'deepseek-chat',
  'deepseek-reasoner',
  'deepseek-flash',
  'deepseek-v4-pro',
]

async function probe(model, effort) {
  const body = {
    model,
    messages: [{ role: 'user', content: '只回答两个字：收到' }],
    stream: true,
    max_tokens: 512,
  }
  if (effort) body.reasoning_effort = effort

  const started = Date.now()
  let res
  try {
    res = await fetch(`${base}/chat/completions`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
      body: JSON.stringify(body),
    })
  } catch (e) {
    return { model, effort, ok: false, note: `网络错误: ${e.message}` }
  }

  if (!res.ok) {
    const text = await res.text().catch(() => '')
    let msg = text.slice(0, 200)
    try {
      msg = JSON.parse(text).error?.message ?? msg
    } catch {
      /* 原样 */
    }
    return { model, effort, ok: false, status: res.status, note: msg }
  }

  const reader = res.body.getReader()
  const dec = new TextDecoder()
  let buf = ''
  let firstAt = 0
  let reasoning = ''
  let content = ''
  let usage = null
  let finish = ''

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += dec.decode(value, { stream: true })
    const parts = buf.split('\n\n')
    buf = parts.pop() ?? ''
    for (const part of parts) {
      const line = part.split('\n').find((l) => l.startsWith('data:'))
      if (!line) continue
      const payload = line.slice(5).trim()
      if (payload === '[DONE]') continue
      let j
      try {
        j = JSON.parse(payload)
      } catch {
        continue
      }
      const d = j.choices?.[0]?.delta ?? {}
      const r = d.reasoning_content ?? d.reasoning ?? ''
      if (r && !firstAt) firstAt = Date.now() - started
      reasoning += r
      content += d.content ?? ''
      if (j.choices?.[0]?.finish_reason) finish = j.choices[0].finish_reason
      if (j.usage) usage = j.usage
    }
  }

  return {
    model,
    effort,
    ok: true,
    ttfb: firstAt ? `${firstAt}ms` : '-',
    total: `${Date.now() - started}ms`,
    cotLen: reasoning.length,
    cotSample: reasoning.slice(0, 40).replace(/\n/g, ' '),
    contentLen: content.length,
    contentSample: content.slice(0, 40).replace(/\n/g, ' '),
    finish,
    usage: usage
      ? `prompt=${usage.prompt_tokens} completion=${usage.completion_tokens} reasoning=${usage.completion_tokens_details?.reasoning_tokens ?? 0}`
      : '（无）',
  }
}

const list = models.length ? models : DEFAULT_MODELS

console.log(`上游: ${base}`)
console.log('\n=== 1. 不带 reasoning_effort ===')
for (const m of list) {
  const r = await probe(m, null)
  if (!r.ok) {
    console.log(`  ${m.padEnd(22)} \x1b[31m失败 HTTP ${r.status ?? '-'}\x1b[0m  ${r.note}`)
  } else {
    const cot = r.cotLen > 0 ? `\x1b[32mCoT ${r.cotLen} 字\x1b[0m` : '\x1b[90m无 CoT\x1b[0m'
    console.log(`  ${m.padEnd(22)} ${cot}  正文 ${r.contentLen} 字  首字 ${r.ttfb}  用量 ${r.usage}`)
    if (r.cotLen > 0) console.log(`      CoT 片段: ${r.cotSample}`)
  }
}

console.log('\n=== 2. 带 reasoning_effort: medium（看上游收不收）===')
for (const m of list) {
  const r = await probe(m, 'medium')
  if (!r.ok) {
    console.log(`  ${m.padEnd(22)} \x1b[33m拒绝 HTTP ${r.status}\x1b[0m  ${r.note}`)
  } else {
    console.log(`  ${m.padEnd(22)} \x1b[32m接受\x1b[0m  CoT ${r.cotLen} 字  正文 ${r.contentLen} 字  用量 ${r.usage}`)
  }
}
