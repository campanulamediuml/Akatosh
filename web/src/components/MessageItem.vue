<script setup lang="ts">
/**
 * 单条消息。
 * - 用户消息：气泡（右侧）；助手消息：CoT 在上、markdown 正文在下
 * - 悬停工具条：复制 / 编辑 / 重新生成 / 删除
 * - 编辑态：markdown 块就地变成自适应高度 textarea，键盘行为与主输入框对齐
 *   （桌面 Enter 保存 / Ctrl+Enter 换行；触屏 Enter 换行 / 点按钮保存），Esc 取消
 * - streaming 显示光标动效并禁用编辑/删除；error 显示红色错误条 + 重试；aborted 显示「已停止」
 * - 底部小字显示 usage（全 0 时不显示）
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, toError } from '@/api/client'
import type { Attachment, Message } from '@/api/types'
import { EMPTY_USAGE } from '@/api/types'
import MarkdownView from './MarkdownView.vue'
import MessageAttachments from './MessageAttachments.vue'
import ReasoningBlock from './ReasoningBlock.vue'
import SearchLogBlock from './SearchLogBlock.vue'
import { useChatStore } from '@/stores/chat'
import { useConversationsStore } from '@/stores/conversations'
import { useSettingsStore } from '@/stores/settings'
import { toast } from '@/stores/toast'
import { copyText } from '@/utils/clipboard'
import { editHint, handleEditKeydown } from '@/utils/editkeys'
import { absoluteTime, cacheDetailText, cachePctText, formatBytes, reasoningTokenText, tokenLine } from '@/utils/format'
import { useThrottledText } from '@/utils/throttle'

const props = defineProps<{ message: Message }>()

const chat = useChatStore()
const convs = useConversationsStore()
const settings = useSettingsStore()

const isUser = computed(() => props.message.role === 'user')
const isAssistant = computed(() => props.message.role === 'assistant')
const isSystem = computed(() => props.message.role === 'system')
const streaming = computed(() => props.message.status === 'streaming')
const usage = computed(() => props.message.usage ?? EMPTY_USAGE)

/**
 * 压缩冻结：id <= conv.compacted_before 的消息已被压进摘要，只读。
 * 冻结的消息不允许编辑/还原/删除/重新生成（改了会和摘要不一致）。
 */
const frozen = computed(() => {
  const cb = chat.conv?.compacted_before ?? 0
  return cb > 0 && props.message.id <= cb
})

/**
 * 流式期间的渲染节流（压测实测：26 万字 CoT 时每帧全量渲染会吃掉 1.2 核 CPU）。
 * 节流只在 streaming 时生效；生成结束的那一刻立即同步最终全文。
 * 节流的是「传给渲染组件的文本」，数据本身（message.content/reasoning）照常每帧更新。
 */
const renderContent = useThrottledText(() => props.message.content ?? '', streaming)
const renderReasoning = useThrottledText(() => props.message.reasoning ?? '', streaming)

/**
 * 消息带的图片。契约：attachments 直接来自后端
 * （state.messages[].attachments 与 SSE 的 message 事件都带），前端不自己拼。
 * 列表里只渲染缩略图，原图由 MessageAttachments → ImageViewer 在点开时才请求。
 */
const attachments = computed<Attachment[]>(() => props.message.attachments ?? [])

/** 消息带的文档（/api/files 传的，read_file 工具可读） */
const files = computed(() => props.message.files ?? [])

const roleLabel = computed(() => (isUser.value ? '你' : isAssistant.value ? 'AI' : '系统'))

/**
 * 这条消息是哪家供应商产的。
 * 多供应商下两个网关可能有同名模型（都叫 gpt-4o），只显示模型名是有歧义的，
 * 所以把供应商名也带上。
 */
const providerLabel = computed(() => {
  if (!props.message.provider_id) return ''
  return settings.providerName(props.message.provider_id)
})

const modelTitle = computed(() =>
  providerLabel.value
    ? `${providerLabel.value} · ${props.message.model_id}`
    : props.message.model_id,
)
const timeText = computed(() => absoluteTime(props.message.created_at))

/** streaming 中的消息不能编辑/删除（后端也会返回 409）；冻结的消息永远不行 */
const canEdit = computed(() => !streaming.value && !isSystem.value && !frozen.value)
const canDelete = computed(() => !streaming.value && !frozen.value)

const hasUsage = computed(() => {
  const u = usage.value
  return u.prompt_tokens > 0 || u.completion_tokens > 0 || u.reasoning_tokens > 0 || u.total_tokens > 0
})

const usageText = computed(() => {
  if (!hasUsage.value) return ''
  // 窄屏下每一寸都贵：分隔用紧凑的「·」；缓存的命中明细拆到 detail 里（窄屏 CSS 藏掉）
  const parts = [
    tokenLine(usage.value),
    reasoningTokenText(usage.value),
    cachePctText(usage.value),
  ]
  return parts.filter(Boolean).join('·')
})

const usageDetail = computed(() => cacheDetailText(usage.value))

const usageTitle = computed(() => {
  const base = `总 tokens ${usage.value.total_tokens}`
  return usageDetail.value ? `${base} · ${usageDetail.value}` : base
})

/* ---------------- 编辑态 ---------------- */

const editing = ref(false)
const draft = ref('')
const saving = ref(false)
const editorEl = ref<HTMLTextAreaElement | null>(null)

function autoGrow(): void {
  const el = editorEl.value
  if (!el) return
  el.style.height = 'auto'
  const max = Math.max(160, Math.round(window.innerHeight * 0.6))
  el.style.height = `${Math.min(el.scrollHeight, max)}px`
}

function startEdit(): void {
  if (!canEdit.value) return
  visible.value = true // 卸载态下先恢复内容，编辑框才有着落点
  draft.value = props.message.content ?? ''
  editing.value = true
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
  if (saving.value || !editing.value) return
  const content = draft.value
  if (content === (props.message.content ?? '')) {
    editing.value = false
    return
  }
  saving.value = true
  const ok = await chat.editMessage(props.message.id, content)
  saving.value = false
  if (ok) editing.value = false
}

async function restoreOriginal(): Promise<void> {
  const ok = await chat.restoreMessage(props.message.id)
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

/* ---------------- 视口外卸载（长会话性能） ---------------- */

/**
 * 长会话的重型内容（markdown 正文 / CoT / 图片）全部渲染会让 DOM 失控
 * （压测里 26 万字 CoT 的会话就是例子）。这里用 IntersectionObserver：
 * 滚出视口 600px 就把重型内容卸载成占位高度，滚回来再恢复。
 * 生成中 / 编辑中的消息绝不卸载；小消息（<300px）不值得卸载。
 * 注意 IO 的可见性判定包含祖先 overflow 裁剪，所以 root 不用显式给 .msglist。
 */
const rootEl = ref<HTMLElement | null>(null)
const visible = ref(true)
const placeholderH = ref(0)
let observer: IntersectionObserver | null = null

/** 这条消息当前允许被卸载吗 */
const canUnload = computed(() => !streaming.value && !editing.value)

onMounted(() => {
  const el = rootEl.value
  if (!el) return
  observer = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (e.isIntersecting) {
          visible.value = true
        } else if (canUnload.value && el.offsetHeight > 300) {
          placeholderH.value = el.offsetHeight
          visible.value = false
        }
      }
    },
    { rootMargin: '600px 0px' },
  )
  observer.observe(el)
})

onBeforeUnmount(() => {
  observer?.disconnect()
  observer = null
})

/* ---------------- 工具条 ---------------- */

async function onCopy(): Promise<void> {
  const ok = await copyText(props.message.content ?? '')
  if (ok) toast.success('已复制')
  else toast.error('复制失败，请手动选择文本')
}

function onRegenerate(): void {
  void chat.regenerate(props.message.id)
}

function onDelete(): void {
  void chat.deleteMessage(props.message.id)
}

/**
 * 从这条消息分叉：把「到这条为止」的历史（含这条）复制成新会话，
 * 标题形如「原标题-消息{id}-fork」。已归档的冻结消息也能 fork
 * （fork 是只读复制，不是修改）。
 */
const forking = ref(false)

async function onFork(): Promise<void> {
  const conv = chat.conv
  if (!conv || forking.value) return
  forking.value = true
  try {
    const res = await api.forkConversation(conv.id, props.message.id)
    const title = res?.conv?.title ?? ''
    toast.success(title ? `已分叉为「${title}」` : '已分叉')
    await convs.refresh()
    if (res?.conv?.id) convs.select(res.conv.id)
  } catch (err) {
    toast.error(`分叉失败：${toError(err).message}`)
  } finally {
    forking.value = false
  }
}
</script>

<template>
  <article
    ref="rootEl"
    class="msg"
    :class="isUser ? 'msg--user' : 'msg--assistant'"
    :data-msg-id="message.id"
    :style="!visible && placeholderH ? { minHeight: placeholderH + 'px' } : undefined"
  >
    <header class="msg__head">
      <span class="msg__role">{{ roleLabel }}</span>
      <span v-if="message.model_id" class="msg__model" :title="modelTitle">
        <span v-if="providerLabel" class="msg__provider">{{ providerLabel }}</span>
        {{ message.model_id }}
      </span>
      <span v-if="message.edited" class="tag msg__edited">已编辑</span>
      <span v-if="timeText">{{ timeText }}</span>
    </header>

    <!-- 视口外卸载：重型内容（CoT / 工具记录 / 正文 / 图片）滚远了才渲染 -->
    <template v-if="visible">
      <!-- CoT 永远在正文上方（流式期间节流渲染，见 renderReasoning） -->
      <ReasoningBlock
        v-if="isAssistant && message.reasoning"
        :text="renderReasoning"
        :message-id="message.id"
        :edited="!!message.reasoning_original"
        :frozen="frozen"
        :streaming="streaming"
        :started-at="message.created_at"
        :finished-at="message.finished_at"
        :tokens="usage.reasoning_tokens"
      />

      <!-- 工具调用：生成中显示实时动作，落地后每次调用一张卡片（带分段 token） -->
      <SearchLogBlock
        v-if="isAssistant"
        :live="chat.searchStatus[message.id] ?? []"
        :args-text="chat.toolArgs[message.id] ?? ''"
        :log-json="message.search_log"
        :seg-usages="message.seg_usages ?? []"
      />
    </template>

    <div v-if="visible" class="msg__body">
      <!-- 编辑态：就地变成 textarea -->
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
          <span class="editor__hint">{{ editHint('Esc 取消') }}</span>
          <button
            v-if="message.edited"
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
        <!-- 用户消息 -->
        <div v-if="isUser" class="msg__bubble">
          <MarkdownView v-if="message.content" :source="message.content" />
          <span v-else-if="attachments.length === 0 && files.length === 0" class="msg__usage">（空消息）</span>
          <MessageAttachments v-if="attachments.length > 0" :attachments="attachments" />
          <div v-if="files.length > 0" class="msg__files">
            <div v-for="f in files" :key="f.id" class="filechip" :title="`文件 #${f.id} · 可用 read_file 读取`">
              <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-doc" />
              </svg>
              <span class="filechip__name">{{ f.name }}</span>
              <span class="filechip__size">{{ formatBytes(f.size) }}</span>
            </div>
          </div>
        </div>

        <!-- 系统消息（一般不会出现，出现了也别丢） -->
        <div v-else-if="isSystem" class="msg__status msg__status--aborted">
          <span class="msg__status-text">{{ message.content }}</span>
        </div>

        <!-- 助手消息（流式期间节流渲染，见 renderContent） -->
        <div v-else class="msg__content">
          <MarkdownView v-if="message.content" :source="renderContent" />
          <span v-else-if="streaming" class="thinking" aria-label="等待回复">
            <i /><i /><i />
          </span>
          <MessageAttachments v-if="attachments.length > 0" :attachments="attachments" />
          <span v-if="streaming" class="cursor" aria-hidden="true" />
        </div>
      </template>
    </div>

    <!-- 失败 / 已停止 -->
    <div v-if="message.status === 'error'" class="msg__status msg__status--error">
      <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
        <use href="#i-alert" />
      </svg>
      <span class="msg__status-text">{{ message.error || '生成失败' }}</span>
      <button type="button" class="btn btn--sm" @click="onRegenerate">重试</button>
    </div>
    <div v-else-if="message.status === 'aborted'" class="msg__status msg__status--aborted">
      <span class="msg__status-text">已停止</span>
      <button type="button" class="btn btn--sm btn--ghost" @click="onRegenerate">重新生成</button>
    </div>

    <footer class="msg__footer">
      <span v-if="usageText" class="msg__usage" :title="usageTitle">
        {{ usageText }}<span v-if="usageDetail" class="msg__usage-detail">（{{ usageDetail }}）</span>
      </span>
      <span v-if="frozen" class="tag msg__frozen" title="这条消息已随上下文压缩归档，不能再修改">已归档</span>
      <!-- 分叉按钮对冻结消息也可用：fork 是只读复制，不改原消息 -->
      <button
        v-if="frozen"
        type="button"
        class="icon-btn"
        title="从这里分叉（复制到这条为止的历史开新分支）"
        :disabled="forking"
        @click="onFork"
      >
        <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-fork" />
        </svg>
      </button>
      <div v-else class="msg__tools">
        <button type="button" class="icon-btn" title="复制正文" @click="onCopy">
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-copy" />
          </svg>
        </button>
        <button
          type="button"
          class="icon-btn"
          title="编辑"
          :disabled="!canEdit"
          @click="startEdit"
        >
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-pencil" />
          </svg>
        </button>
        <button
          type="button"
          class="icon-btn"
          title="重新生成（截断这条之后的消息）"
          :disabled="streaming"
          @click="onRegenerate"
        >
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-refresh" />
          </svg>
        </button>
        <button
          type="button"
          class="icon-btn"
          title="从这里分叉（复制到这条为止的历史开新分支）"
          :disabled="forking"
          @click="onFork"
        >
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-fork" />
          </svg>
        </button>
        <button
          type="button"
          class="icon-btn icon-btn--danger"
          title="删除"
          :disabled="!canDelete"
          @click="onDelete"
        >
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-trash" />
          </svg>
        </button>
      </div>
    </footer>
  </article>
</template>
