<script setup lang="ts">
/**
 * 顶部轻量 toast（自己写，不引库）。挂在 App.vue 最外层，读 stores/toast.ts。
 * 用途：409 busy 丢弃提示、保存成功、后端错误等。
 */
import { dismiss, toast } from '@/stores/toast'
import type { ToastKind } from '@/stores/toast'

const ICONS: Record<ToastKind, string> = {
  info: 'i-sparkle',
  success: 'i-check',
  error: 'i-alert',
}

function iconFor(kind: ToastKind): string {
  return ICONS[kind] ?? 'i-sparkle'
}
</script>

<template>
  <div class="toasts" role="status" aria-live="polite">
    <div v-for="t in toast.items" :key="t.id" class="toast" :class="`toast--${t.kind}`">
      <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
        <use :href="`#${iconFor(t.kind)}`" />
      </svg>
      <span class="toast__text">{{ t.text }}</span>
      <button type="button" class="toast__close" aria-label="关闭提示" @click="dismiss(t.id)">
        <svg width="11" height="11" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-close" />
        </svg>
      </button>
    </div>
  </div>
</template>
