<script setup lang="ts">
/**
 * 灯箱：全屏看原图（契约 §4）。
 *
 * 性能硬约束（改之前先读 API.md §4「前端硬约束」，别改回去）：
 * - **`raw_url` 全项目只在这里被引用**：组件只在用户点开图片时才挂载，
 *   也就是说只有这一刻浏览器才会去请求原图；
 * - 不做邻居预加载：没有隐藏的 `<img>`、没有 `new Image()`。多图消息里
 *   偷偷预热几张几 MB 的原图，正是要避免的事；
 * - 关掉再打开是 0 请求：后端对 `/raw` 发的是 immutable 长缓存，浏览器直接命中。
 *
 * 交互：
 * - 关闭：点遮罩、点右上角 ✕、按 Esc（三者都只关灯箱，不穿透到下面的消息）；
 * - 多张图时左右切换：键盘 ← → 与两侧按钮（循环）；
 * - 顶部显示「原图尺寸 · 文件名」，右上角可「在新标签页打开」。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { Attachment } from '@/api/types'
import { formatBytes } from '@/utils/format'

const props = defineProps<{
  /** 同一批附件（一条消息里的图片），左右切换在它内部进行 */
  attachments: Attachment[]
  /** 当前看的是第几张 */
  index: number
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'update:index', index: number): void
}>()

const count = computed(() => props.attachments.length)
const many = computed(() => count.value > 1)

/**
 * 当前这一张。**整个组件里只有这个 computed 的 raw_url 会被绑定到 img 上**，
 * 别的图片在这份 DOM 里根本不存在 → 也就不会被请求。
 */
const current = computed<Attachment | null>(() => props.attachments[props.index] ?? null)

const imgEl = ref<HTMLImageElement | null>(null)
const loading = ref(true)
const failed = ref(false)

/** 例如 "1600×1200 · shot.png"（宽高未知时只显示文件名） */
const meta = computed(() => {
  const a = current.value
  if (!a) return ''
  const dim = a.width > 0 && a.height > 0 ? `${a.width}×${a.height}` : ''
  return [dim, a.name].filter(Boolean).join(' · ')
})

const detailTitle = computed(() => {
  const a = current.value
  if (!a) return ''
  return [a.name, formatBytes(a.size), a.mime].filter(Boolean).join(' · ')
})

const rawUrl = computed(() => current.value?.raw_url ?? '')

/**
 * 缓存命中时 `load` 有可能在监听器挂上之前就已经发生过，
 * 补一次同步检查，避免 spinner 一直转。
 */
function checkComplete(): void {
  const el = imgEl.value
  if (el && el.complete && el.naturalWidth > 0) {
    loading.value = false
    failed.value = false
  }
}

// 换一张：raw_url 变了，重新进入 loading（这一次才会去请求新原图）
watch(
  () => current.value?.id,
  async () => {
    loading.value = true
    failed.value = false
    await nextTick()
    checkComplete()
  },
)

function onLoad(): void {
  loading.value = false
  failed.value = false
}

function onError(): void {
  loading.value = false
  failed.value = true
}

/** 左右切换，循环 */
function step(delta: number): void {
  if (!many.value) return
  const n = count.value
  emit('update:index', (props.index + delta + n) % n)
}

function onKeydown(e: KeyboardEvent): void {
  if (e.key === 'Escape') {
    e.preventDefault()
    emit('close')
    return
  }
  if (e.key === 'ArrowLeft') {
    e.preventDefault()
    step(-1)
    return
  }
  if (e.key === 'ArrowRight') {
    e.preventDefault()
    step(1)
  }
}

onMounted(() => {
  // 捕获阶段：Esc / ← → 优先归灯箱，别落到页面上别的快捷键上
  document.addEventListener('keydown', onKeydown, true)
  checkComplete()
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown, true)
})
</script>

<template>
  <!-- Teleport 到 body：遮罩必须盖住整个视口，且不受消息区任何定位上下文影响 -->
  <Teleport to="body">
    <div
      class="viewer"
      role="dialog"
      aria-modal="true"
      :aria-label="meta || '图片预览'"
      @click.self="emit('close')"
    >
      <header class="viewer__bar" @click.stop>
        <span class="viewer__meta" :title="detailTitle">{{ meta }}</span>
        <span class="viewer__spacer" />
        <a
          v-if="rawUrl"
          class="viewer__open"
          :href="rawUrl"
          target="_blank"
          rel="noopener noreferrer"
          title="在新标签页打开原图"
        >
          <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-external" />
          </svg>
          在新标签页打开
        </a>
        <button type="button" class="icon-btn viewer__close" title="关闭（Esc）" @click="emit('close')">
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-close" />
          </svg>
        </button>
      </header>

      <button
        v-if="many"
        type="button"
        class="viewer__nav viewer__nav--prev"
        title="上一张（←）"
        @click.stop="step(-1)"
      >
        <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-chevron-right" />
        </svg>
      </button>

      <!-- 内容区自己 stop：点图片本身不关闭，只有点遮罩才关 -->
      <div class="viewer__stage" @click.stop>
        <img
          v-if="current && !failed"
          ref="imgEl"
          class="viewer__img"
          :class="{ 'viewer__img--loading': loading }"
          :src="rawUrl"
          :alt="current.name"
          decoding="async"
          @load="onLoad"
          @error="onError"
        />

        <div v-if="current && loading && !failed" class="viewer__hint">
          <span class="spinner" />
          正在加载原图…
        </div>

        <div v-else-if="failed || !current" class="viewer__hint viewer__hint--error">
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-alert" />
          </svg>
          <span>{{ current ? '原图加载失败' : '这张图片已经不存在了' }}</span>
          <a v-if="rawUrl" :href="rawUrl" target="_blank" rel="noopener noreferrer">在新标签页打开</a>
        </div>
      </div>

      <button
        v-if="many"
        type="button"
        class="viewer__nav viewer__nav--next"
        title="下一张（→）"
        @click.stop="step(1)"
      >
        <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-chevron-right" />
        </svg>
      </button>

      <div v-if="many" class="viewer__counter" @click.stop>{{ index + 1 }} / {{ count }}</div>
    </div>
  </Teleport>
</template>
