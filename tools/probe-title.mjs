/**
 * 诊断：非流式请求下，推理模型的 content 会不会被 CoT 吃光导致为空。
 *
 *   node tools/probe-title.mjs <baseUrl> <apiKey> <model>
 */
const base = (process.argv[2] || 'https://api.deepseek.com/v1').replace(/\/+$/, '')
const key = process.argv[3]
const model = process.argv[4] || 'deepseek-flash'

const prompt = `请给下面这段对话起一个标题。

要求：
- 不超过 15 个字，要具体，能一眼看出这段对话在讲什么
- 只输出标题本身：不要引号、不要书名号、不要结尾标点、不要任何解释或前缀
- 用对话所用的语言

对话内容：
用户：请详细讲讲 Go 的并发模型，goroutine 和 channel 到底怎么配合
助手：Go 的并发模型核心是 goroutine 和 channel……

标题：`

async function run(label, opts) {
  const body = {
    model,
    messages: [{ role: 'user', content: prompt }],
    stream: false,
    ...opts,
  }
  const t0 = Date.now()
  const res = await fetch(`${base}/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
    body: JSON.stringify(body),
  })
  const text = await res.text()
  const ms = Date.now() - t0
  if (!res.ok) {
    console.log(`  ${label.padEnd(34)} \x1b[31mHTTP ${res.status}\x1b[0m  ${text.slice(0, 140)}`)
    return
  }
  const j = JSON.parse(text)
  const m = j.choices?.[0]?.message ?? {}
  const content = m.content ?? ''
  const reason = m.reasoning_content ?? m.reasoning ?? ''
  const u = j.usage ?? {}
  const rt = u.completion_tokens_details?.reasoning_tokens ?? 0
  const verdict = content.trim() ? `\x1b[32mcontent=${JSON.stringify(content.trim().slice(0, 30))}\x1b[0m` : '\x1b[31mcontent 是空的！\x1b[0m'
  console.log(
    `  ${label.padEnd(34)} ${verdict}\n` +
      `      ${ms}ms  finish=${j.choices?.[0]?.finish_reason}  CoT=${reason.length} 字  ` +
      `completion=${u.completion_tokens ?? '-'} (其中 reasoning=${rt})  max_tokens=${opts.max_tokens ?? '(未传)'}`,
  )
}

console.log(`上游 ${base}   模型 ${model}\n`)
await run('max_tokens=512（当前实现）', { max_tokens: 512 })
await run('max_tokens=2048', { max_tokens: 2048 })
await run('不传 max_tokens', {})
await run('max_tokens=512 + effort=none', { max_tokens: 512, reasoning_effort: 'none' })
await run('max_tokens=512 + effort=minimal', { max_tokens: 512, reasoning_effort: 'minimal' })
