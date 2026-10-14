<script setup lang="ts">
/**
 * CoT 折叠块（v2：数据直接来自 message.reasoning / usage）。
 *
 * - 标题栏：「思考过程 · 3.2s · 1,204 tokens」
 * - 流式期间自动展开并逐字刷新（内容由父级按帧 flush 进来）；
 *   生成结束后保持展开（用户正看着这次生成）
 * - **历史消息（重新进入页面）默认折叠**——CoT 动辄几千上万字，
 *   全展开布局就散了；只是渲染逻辑，数据不受影响
 * - 用户手动折叠过就不再自动展开（userToggled 标记）
 * - usage.reasoning_tokens 要等生成结束才由上游给出，流式期间显示 ≈ 估算值
 * - CoT 可编辑：编辑后的 reasoning 会随历史回传给上游（DeepSeek 思考模式规则），
 *   改 CoT 就是在引导模型后续的思路；原文保留，可一键还原
 */
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import MarkdownView from './MarkdownView.vue'
import { formatDuration, formatNumber } from '@/utils/format'
import { editHint, handleEditKeydown } from '@/utils/editkeys'
import { useChatStore } from '@/stores/chat'

const props = withDefaults(
  defineProps<{
    text: string
    /** 消息 id（编辑 CoT 用） */
    messageId?: number
    /** CoT 是否被编辑过（显示「已编辑」和「还原原文」） */
    edited?: boolean
    /** 压缩冻结（id <= compacted_before）：不显示编辑入口 */
    frozen?: boolean
    /** 该消息是否正在流式输出 */
    streaming?: boolean
    /** Unix 毫秒 */
    startedAt?: number
    /** Unix 毫秒，0 表示还没结束 */
    finishedAt?: number
    /** usage.reasoning_tokens（结束时才有） */
    tokens?: number
    /** 默认展开状态（首次挂载时生效） */
    defaultOpen?: boolean
  }>(),
  { streaming: false, startedAt: 0, finishedAt: 0, tokens: 0, defaultOpen: false, messageId: 0, edited: false, frozen: false },
)

const chat = useChatStore()

const open = ref(props.defaultOpen)
/** 用户是否手动干预过折叠状态 */
const userToggled = ref(false)
/** 流式期间用它驱动耗时刷新 */
const now = ref(Date.now())

/* ---------------- CoT 编辑 ---------------- */

const editing = ref(false)
const draft = ref('')
const saving = ref(false)
const editorEl = ref<HTMLTextAreaElement | null>(null)

function autoGrow(): void {
  const el = editorEl.value
  if (!el) return
  el.style.height = 'auto'
  const max = Math.max(200, Math.round(window.innerHeight * 0.6))
  el.style.height = `${Math.min(el.scrollHeight, max)}px`
}

function startEdit(): void {
  if (props.streaming || !props.messageId) return
  draft.value = props.text ?? ''
  editing.value = true
  open.value = true
  void nextTick(() => {
    autoGrow()
    editorEl.value?.focus()
    const len = editorEl.value?.value.length ?? 0
    editorEl.value?.setSelectionRange(len, len)
  })
}

function cancelEdit(): void {
  editing.value = false
}

async function saveEdit(): Promise<void> {
  if (saving.value || !props.messageId) return
  saving.value = true
  const ok = await chat.editReasoning(props.messageId, draft.value)
  saving.value = false
  if (ok) editing.value = false
}

async function restoreOriginal(): Promise<void> {
  if (!props.messageId) return
  const ok = await chat.restoreReasoning(props.messageId)
  if (ok) editing.value = false
}

function onEditorKeydown(e: KeyboardEvent): void {
  if (e.key === 'Escape') {
    e.preventDefault()
    cancelEdit()
    return
  }
  // Enter 行为与主输入框对齐（utils/editkeys.ts 里有中文输入法守卫）
  handleEditKeydown(e, () => void saveEdit())
}

let ticker: ReturnType<typeof setInterval> | null = null

function stopTicker(): void {
  if (ticker !== null) {
    clearInterval(ticker)
    ticker = null
  }
}

watch(
  () => props.streaming,
  (streaming) => {
    if (streaming) {
      if (!userToggled.value) open.value = true
      if (ticker === null) {
        ticker = setInterval(() => {
          now.value = Date.now()
        }, 100)
      }
    } else {
      stopTicker()
      now.value = Date.now()
      // 结束后保持展开，不做自动折叠
    }
  },
  { immediate: true },
)

onBeforeUnmount(stopTicker)

function toggle(): void {
  open.value = !open.value
  userToggled.value = true
}

/** 耗时：流式中走实时，结束后用 finished_at 定格 */
const elapsedText = computed(() => {
  const start = props.startedAt
  if (!start) return ''
  const end = props.streaming || !props.finishedAt ? now.value : props.finishedAt
  return formatDuration(Math.max(0, (end - start) / 1000))
})

/** 结束后用真实 usage；流式期间上游还没给，按字符粗估（带 ≈ 前缀，不冒充真实值） */
const tokenText = computed(() => {
  if (props.tokens > 0) return `${formatNumber(props.tokens)} tokens`
  if (props.streaming && props.text.length > 0) {
    return `≈${formatNumber(estimateTokens(props.text))} tokens`
  }
  return ''
})

/** 粗估：CJK 字符约 1 token，其余按 4 字符 1 token */
function estimateTokens(text: string): number {
  let cjk = 0
  for (const ch of text) {
    const code = ch.codePointAt(0) ?? 0
    if (code >= 0x2e80 && code <= 0x9fff) cjk += 1
  }
  return Math.max(1, Math.round(cjk + (text.length - cjk) / 4))
}

const hasText = computed(() => (props.text ?? '').length > 0)
</script>

<template>
  <section class="reasoning" :class="{ 'reasoning--open': open }">
    <button type="button" class="reasoning__head" :aria-expanded="open" @click="toggle">
      <span class="reasoning__caret" aria-hidden="true">
        <svg width="12" height="12" viewBox="0 0 16 16">
          <use href="#i-chevron-right" />
        </svg>
      </span>
      <span class="reasoning__label">思考过程</span>
      <span v-if="edited" class="tag msg__edited">已编辑</span>
      <span v-if="elapsedText" class="reasoning__stats">· {{ elapsedText }}</span>
      <span v-if="tokenText" class="reasoning__stats">· {{ tokenText }}</span>
      <span class="reasoning__spacer" />
      <span v-if="streaming && hasText" class="pulse" aria-label="正在思考" />
      <span v-else-if="streaming" class="reasoning__stats">思考中…</span>
      <span v-else class="reasoning__stats">{{ open ? '收起' : '展开' }}</span>
    </button>

    <div v-if="open" class="reasoning__body">
      <!-- 编辑态：就地变成 textarea（编辑后会随历史回传给上游） -->
      <div v-if="editing" class="editor">
        <textarea
          ref="editorEl"
          v-model="draft"
          class="editor__area"
          spellcheck="false"
          @input="autoGrow"
          @keydown="onEditorKeydown"
        />
        <div class="editor__bar">
          <span class="editor__hint">{{ editHint('Esc 取消 · 编辑后会回传给模型') }}</span>
          <button
            v-if="edited"
            type="button"
            class="btn btn--sm"
            :disabled="saving"
            @click="restoreOriginal"
          >
            还原原文
          </button>
          <button type="button" class="btn btn--sm" :disabled="saving" @click="cancelEdit">取消</button>
          <button type="button" class="btn btn--sm btn--primary" :disabled="saving" @click="saveEdit">
            保存
          </button>
        </div>
      </div>

      <template v-else>
        <MarkdownView v-if="hasText" :source="text" compact />
        <p v-else class="reasoning__stats" style="margin: 0">（暂无内容）</p>
        <div v-if="!streaming && messageId && hasText && !frozen" class="reasoning__editbar">
          <button type="button" class="icon-btn" title="编辑 CoT（修改后会随历史回传给模型）" @click.stop="startEdit">
            <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-pencil" />
            </svg>
          </button>
        </div>
      </template>
    </div>
  </section>
</template>

<style scoped>
/* 编辑入口：安静地待在 CoT 右下角，不抢视线 */
.reasoning__editbar {
  display: flex;
  justify-content: flex-end;
  margin-top: var(--sp-1);
  opacity: 0.35;
  transition: opacity 0.15s;
}

.reasoning:hover .reasoning__editbar {
  opacity: 1;
}

@media (hover: none) {
  .reasoning__editbar {
    opacity: 0.7;
  }
}
</style>

