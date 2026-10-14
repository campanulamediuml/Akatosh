/**
 * 关键问题：DeepSeek 的**流式**响应里，usage 到底报不报缓存命中/未命中？
 *
 *   node tools/probe-cache.mjs <baseUrl> <apiKey> <model>
 *
 * 做法：先用一段很长的 system 前缀发一次（建立缓存），等一会儿，
 * 再用**完全相同**的前缀发第二次（应当命中）。两次都开 stream，
 * 把最后那条 usage 原样打印出来。
 */
const base = (process.argv[2] || 'https://api.deepseek.com/v1').replace(/\/+$/, '')
const key = process.argv[3]
const model = process.argv[4] || 'deepseek-flash'

// 造一个足够长的前缀（远超缓存最小粒度 64 token）
const prefix =
  '以下是一份长期有效的背景资料，请把它当作事实记住，后续回答都基于它：\n' +
  Array.from({ length: 40 }, (_, i) =>
    `第 ${i + 1} 条：Go 语言的运行时调度器采用 GMP 模型，G 是 goroutine，M 是内核线程，P 是逻辑处理器；` +
    `channel 有缓冲与无缓冲之分，select 可以多路复用，sync 包提供 Mutex、WaitGroup、Once 等原语。`,
  ).join('\n')

async function ask(label, question) {
  const res = await fetch(`${base}/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
    body: JSON.stringify({
      model,
      messages: [
        { role: 'system', content: prefix },
        { role: 'user', content: question },
      ],
      stream: true,
      max_tokens: 32,
      reasoning_effort: 'none',
    }),
  })
  if (!res.ok) {
    console.log(`  ${label}: HTTP ${res.status} ${(await res.text()).slice(0, 160)}`)
    return
  }
  const reader = res.body.getReader()
  const dec = new TextDecoder()
  let buf = ''
  let lastUsage = null
  let sawCacheField = false
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
      try {
        const j = JSON.parse(payload)
        if (j.usage) {
          lastUsage = j.usage
          if ('prompt_cache_hit_tokens' in j.usage || 'prompt_cache_miss_tokens' in j.usage) {
            sawCacheField = true
          }
        }
      } catch {
        /* 忽略 */
      }
    }
  }
  console.log(`\n=== ${label} ===`)
  console.log('  流式响应里出现缓存字段:', sawCacheField ? '\x1b[32m是\x1b[0m' : '\x1b[31m否\x1b[0m')
  console.log('  最后一条 usage:')
  console.log(
    JSON.stringify(lastUsage, null, 2)
      .split('\n')
      .map((l) => '    ' + l)
      .join('\n'),
  )
}

await ask('第一次（建立缓存）', '用一句话说明 G 是什么。')
console.log('\n等待 20 秒让缓存生效…')
await new Promise((r) => setTimeout(r, 20000))
await ask('第二次（相同前缀，应当命中）', '用一句话说明 P 是什么。')
