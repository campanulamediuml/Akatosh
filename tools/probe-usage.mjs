/**
 * 探测真实上游返回的 usage 结构：缓存命中/未命中到底叫什么字段、有没有值。
 *
 *   node tools/probe-usage.mjs <baseUrl> <apiKey> <model>
 */
const base = (process.argv[2] || 'https://api.deepseek.com/v1').replace(/\/+$/, '')
const key = process.argv[3]
const model = process.argv[4] || 'deepseek-flash'

async function ask(label, messages) {
  const res = await fetch(`${base}/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
    body: JSON.stringify({ model, messages, stream: false, max_tokens: 64 }),
  })
  const j = await res.json()
  console.log(`\n=== ${label} ===`)
  console.log('  usage 原始结构:')
  console.log(
    JSON.stringify(j.usage, null, 2)
      .split('\n')
      .map((l) => '    ' + l)
      .join('\n'),
  )
}

// 第一发：长 system 前缀（大概率未命中缓存）
const longPrefix =
  '你是一个严谨的技术助手。以下是背景资料，请记住它：' +
  'Go 语言由 Google 在 2009 年发布，主打并发与简洁。goroutine 是用户态轻量线程，' +
  'channel 是 goroutine 之间的通信方式，select 用于多路复用。运行时调度器 GMP 模型……'.repeat(3)

await ask('第一次请求（大概率未命中缓存）', [
  { role: 'system', content: longPrefix },
  { role: 'user', content: '用一句话回答：goroutine 是什么？' },
])

await new Promise((r) => setTimeout(r, 1200))

await ask('第二次相同前缀（应当命中缓存）', [
  { role: 'system', content: longPrefix },
  { role: 'user', content: '用一句话回答：channel 是什么？' },
])
