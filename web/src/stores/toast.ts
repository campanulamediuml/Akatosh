/**
 * 20 行 toast：不引任何库。App.vue 里挂一个 ToastHost 渲染即可。
 */

import { reactive } from 'vue'

export type ToastKind = 'info' | 'success' | 'error'

export interface Toast {
  id: number
  kind: ToastKind
  text: string
}

let seq = 0
const state = reactive<{ items: Toast[] }>({ items: [] })

function push(kind: ToastKind, text: string, durationMs: number): number {
  const id = ++seq
  state.items.push({ id, kind, text })
  if (durationMs > 0) {
    setTimeout(() => dismiss(id), durationMs)
  }
  return id
}

export function dismiss(id: number): void {
  const idx = state.items.findIndex((t) => t.id === id)
  if (idx >= 0) state.items.splice(idx, 1)
}

export const toast = {
  items: state.items,
  info: (text: string, durationMs = 2600) => push('info', text, durationMs),
  success: (text: string, durationMs = 2600) => push('success', text, durationMs),
  /** 错误信息停留久一点，方便看清后端的原始 message */
  error: (text: string, durationMs = 6000) => push('error', text, durationMs),
  dismiss,
}
