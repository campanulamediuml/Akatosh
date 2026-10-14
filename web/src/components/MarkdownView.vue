<script setup lang="ts">
/**
 * 渲染一段 markdown（已消毒），并给代码块挂「复制」按钮的行为。
 * 复制按钮是 enhanceCodeBlocks 注入到 HTML 里的，用事件委托处理。
 */
import { computed, onBeforeUnmount, ref } from 'vue'
import { renderMarkdownWithCodeBar } from '@/utils/markdown'
import { toast } from '@/stores/toast'

const props = withDefaults(
  defineProps<{
    /** markdown 原文 */
    source: string
    /** 紧凑模式（CoT 里用），字号小一点 */
    compact?: boolean
  }>(),
  { compact: false },
)

const el = ref<HTMLElement | null>(null)
const html = computed(() => renderMarkdownWithCodeBar(props.source ?? ''))

/** 最近一次「已复制」的按钮，用于短暂反馈 */
let copiedTimer: ReturnType<typeof setTimeout> | null = null

function flash(button: HTMLElement): void {
  button.classList.add('is-copied')
  const original = button.textContent
  button.textContent = '已复制'
  if (copiedTimer) clearTimeout(copiedTimer)
  copiedTimer = setTimeout(() => {
    button.classList.remove('is-copied')
    button.textContent = original
    copiedTimer = null
  }, 1400)
}

/** 优先用 navigator.clipboard，非安全上下文（http + 非 localhost）降级到 execCommand */
async function writeClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      /* 落到降级分支 */
    }
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
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

async function onClick(e: MouseEvent): Promise<void> {
  const target = e.target as HTMLElement | null
  const button = target?.closest?.('.code-block__copy') as HTMLElement | null
  if (!button) return
  e.preventDefault()
  const text = button.dataset.copy ?? ''
  const ok = await writeClipboard(text)
  if (ok) {
    flash(button)
  } else {
    toast.error('复制失败，请手动选择文本')
  }
}

onBeforeUnmount(() => {
  if (copiedTimer) clearTimeout(copiedTimer)
})
</script>

<template>
  <div
    ref="el"
    class="markdown"
    :class="{ 'markdown--compact': compact }"
    v-html="html"
    @click="onClick"
  />
</template>

<style scoped>
.markdown--compact {
  font-size: var(--fs-md);
  line-height: 1.65;
}

.markdown--compact :deep(.code-block__pre code) {
  font-size: var(--fs-sm);
}
</style>
