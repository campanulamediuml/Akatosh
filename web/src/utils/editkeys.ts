/**
 * 编辑框的键盘行为，与主输入框（Composer.vue）对齐：
 * - 桌面（有实体键盘）：裸 Enter = 保存；Ctrl/Cmd/Alt+Enter = 换行（浏览器默认
 *   不会为这组组合键换行，要手动插）；Shift+Enter = 换行（浏览器默认行为）
 * - 触屏（hover:none，软键盘没有修饰键）：Enter 只换行，保存走按钮
 * - 中文输入法守卫：组字过程（isComposing / keyCode 229）的按键一律放行
 *
 * Escape 各组件语义不同（取消编辑 / 关弹窗），留在各自的处理器里。
 */

/** 与 CSS 触屏断点同一个条件，保证行为与样式同进同退 */
export const coarsePointer = window.matchMedia('(hover: none)').matches

/** 在光标处插入换行并触发 input 事件（让 v-model 拿到新值） */
function insertNewlineAtCursor(el: HTMLTextAreaElement | null): void {
  if (!el) return
  const start = el.selectionStart ?? el.value.length
  const end = el.selectionEnd ?? start
  el.value = `${el.value.slice(0, start)}\n${el.value.slice(end)}`
  el.selectionStart = el.selectionEnd = start + 1
  el.dispatchEvent(new Event('input', { bubbles: true }))
}

/** 编辑器统一的 Enter 处理：保存动作由调用方给 */
export function handleEditKeydown(e: KeyboardEvent, onSave: () => void): void {
  // 中文输入法守卫（硬性要求）：组字过程中的回车一律放行
  if (e.isComposing || e.keyCode === 229) return
  if (e.key !== 'Enter') return

  const withModifier = e.ctrlKey || e.metaKey || e.shiftKey || e.altKey
  if (!withModifier) {
    // 触屏：裸 Enter 交给浏览器默认行为（插入换行），保存走按钮
    if (coarsePointer) return
    // 桌面：裸 Enter 保存
    e.preventDefault()
    onSave()
    return
  }

  // Shift+Enter 浏览器默认就会插入换行，交给它即可
  if (e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) return

  // Ctrl/Cmd/Alt+Enter 不会触发浏览器的换行默认行为，手动插
  e.preventDefault()
  insertNewlineAtCursor(e.target as HTMLTextAreaElement)
}

/** 编辑器提示文案（桌面/触屏两套） */
export function editHint(extra = ''): string {
  const base = coarsePointer ? '回车换行 · 点按钮保存' : 'Enter 保存 · Ctrl+Enter 换行'
  return extra ? `${base} · ${extra}` : base
}
