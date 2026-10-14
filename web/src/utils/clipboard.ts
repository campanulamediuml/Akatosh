/**
 * 复制到剪贴板。
 * 优先 navigator.clipboard；非安全上下文（http + 非 localhost）降级到 execCommand，
 * 两种都失败时返回 false，让调用方提示「请手动选择文本」。
 */

export async function copyText(text: string): Promise<boolean> {
  const value = text ?? ''
  if (!value) return false

  if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    try {
      await navigator.clipboard.writeText(value)
      return true
    } catch {
      /* 落到降级分支 */
    }
  }

  try {
    const ta = document.createElement('textarea')
    ta.value = value
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.top = '0'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}
