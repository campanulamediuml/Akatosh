/**
 * 时间 / 数字格式化（自己写，不引 dayjs）。
 * 契约里所有时间戳都是 Unix 毫秒整数。
 */

const SECOND = 1000
const MINUTE = 60 * SECOND
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

function pad2(n: number): string {
  return n < 10 ? `0${n}` : String(n)
}

/** 后端可能给 0（未知时间），统一按"未知"处理 */
export function isValidTs(ts: number | null | undefined): ts is number {
  return typeof ts === 'number' && Number.isFinite(ts) && ts > 0
}

/**
 * 相对时间：刚刚 / 3 分钟前 / 2 小时前 / 昨天 / 3 天前 / 2025-01-02
 */
export function relativeTime(ts: number, now: number = Date.now()): string {
  if (!isValidTs(ts)) return ''
  const diff = now - ts
  if (diff < 0) return '刚刚'
  if (diff < 45 * SECOND) return '刚刚'
  if (diff < HOUR) return `${Math.max(1, Math.floor(diff / MINUTE))} 分钟前`
  if (diff < DAY) return `${Math.floor(diff / HOUR)} 小时前`

  const days = Math.floor(diff / DAY)
  if (days === 1) return '昨天'
  if (days < 7) return `${days} 天前`
  if (days < 30) return `${Math.floor(days / 7)} 周前`

  const d = new Date(ts)
  const nowD = new Date(now)
  if (d.getFullYear() === nowD.getFullYear()) {
    return `${d.getMonth() + 1}月${d.getDate()}日`
  }
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
}

/** 绝对时间：2025-01-02 15:04 */
export function absoluteTime(ts: number): string {
  if (!isValidTs(ts)) return ''
  const d = new Date(ts)
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(
    d.getMinutes(),
  )}`
}

/** 只有时分秒：15:04:09 */
export function clockTime(ts: number): string {
  if (!isValidTs(ts)) return ''
  const d = new Date(ts)
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
}

/**
 * 耗时秒数，最多一位小数：3.2
 * 用 (end - start) / 1000 计算，负数按 0 处理。
 */
export function durationSeconds(startTs: number, endTs: number): number {
  if (!isValidTs(startTs)) return 0
  const end = isValidTs(endTs) ? endTs : Date.now()
  return Math.max(0, (end - startTs) / 1000)
}

/** 3.2s / 0.4s */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0.0s'
  if (seconds < 60) return `${seconds.toFixed(1)}s`
  const m = Math.floor(seconds / 60)
  const s = Math.round(seconds % 60)
  return `${m}m ${s}s`
}

/** 千分位：1204 -> 1,204 */
export function formatNumber(n: number | null | undefined): string {
  if (typeof n !== 'number' || !Number.isFinite(n)) return '0'
  // 不加千分位逗号：token 数本来就是要读的量级，逗号只增加视觉噪音
  return String(Math.trunc(n))
}

/** 大数缩写：12345 -> 12.3k（用于 context_len 之类） */
export function abbreviateNumber(n: number | null | undefined): string {
  if (typeof n !== 'number' || !Number.isFinite(n)) return '0'
  if (Math.abs(n) < 1000) return String(Math.trunc(n))
  if (Math.abs(n) < 1_000_000) return `${(n / 1000).toFixed(n % 1000 === 0 ? 0 : 1)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

/** 附件字节数：900 -> "900 B"，1536 -> "1.5 KB"，5765120 -> "5.5 MB"；非法值返回空串 */
export function formatBytes(n: number | null | undefined): string {
  if (typeof n !== 'number' || !Number.isFinite(n) || n <= 0) return ''
  if (n < 1024) return `${Math.round(n)} B`
  const kb = n / 1024
  if (kb < 1024) return `${kb < 10 ? kb.toFixed(1) : String(Math.round(kb))} KB`
  const mb = kb / 1024
  if (mb < 1024) return `${mb < 10 ? mb.toFixed(1) : String(Math.round(mb))} MB`
  return `${(mb / 1024).toFixed(1)} GB`
}

/**
 * usage 展示用的字段形状。
 * 注意字段名必须与后端 Usage（prompt_tokens / completion_tokens / reasoning_tokens /
 * total_tokens）一致 —— 下面两个函数就是按这套名字取值读的。
 */
export interface TokenParts {
  prompt_tokens: number
  completion_tokens: number
  reasoning_tokens: number
  total_tokens: number
  /** 前缀缓存命中/未命中（上游不给就是 0） */
  cache_hit_tokens?: number
  cache_miss_tokens?: number
}

/**
 * usage 展示文本的几个片段，交给模板拼：
 *   上行 "↑120 ↓800"
 *   思考标记仅当 reasoning_tokens > 0 时出现
 *   缓存标记仅当上游确实报了缓存数据时出现
 */
export function tokenLine(usage: TokenParts | null | undefined): string {
  if (!usage) return ''
  const up = formatNumber(usage.prompt_tokens)
  const down = formatNumber(usage.completion_tokens)
  return `↑${up} ↓${down}`
}

export function reasoningTokenText(usage: TokenParts | null | undefined): string {
  if (!usage || !usage.reasoning_tokens) return ''
  return `思考${formatNumber(usage.reasoning_tokens)}`
}

/**
 * 前缀缓存提示（紧凑版）：只给命中率百分比。
 * 缓存命中部分便宜一个数量级，所以这行对判断成本很有用。
 * 上游没报缓存数据（两个数都是 0）时返回空串，不占地方。
 */
export function cachePctText(usage: TokenParts | null | undefined): string {
  if (!usage) return ''
  const hit = usage.cache_hit_tokens ?? 0
  const miss = usage.cache_miss_tokens ?? 0
  if (hit + miss === 0) return ''
  return `缓存${Math.round((hit / (hit + miss)) * 100)}%`
}

/**
 * 缓存命中/未命中的明细数字。宽屏下跟在百分比后面，窄屏被 CSS 藏掉
 * （完整信息始终在 title 悬浮里）。
 */
export function cacheDetailText(usage: TokenParts | null | undefined): string {
  if (!usage) return ''
  const hit = usage.cache_hit_tokens ?? 0
  const miss = usage.cache_miss_tokens ?? 0
  if (hit + miss === 0) return ''
  return `命中 ${formatNumber(hit)} / 未命中 ${formatNumber(miss)}`
}

/** 输入框字符数展示：1.2k */
export function charCount(n: number): string {
  return n < 1000 ? String(n) : abbreviateNumber(n)
}
