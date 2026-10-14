<script setup lang="ts">
/**
 * 消息区。
 * 自动滚动规则（契约 §5.5）：只有用户**本来就贴着底部**（距底 < 80px）时才跟随；
 * 用户往上翻时绝不抢滚动条，并在右下角显示「回到底部」。
 */
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useChatStore } from '@/stores/chat'
import { useConversationsStore } from '@/stores/conversations'
import MessageItem from './MessageItem.vue'

const chat = useChatStore()
const convs = useConversationsStore()

/** 距底小于它就算「贴着底部」 */
const STICK_THRESHOLD = 80

const scroller = ref<HTMLElement | null>(null)
const atBottom = ref(true)

const hasMessages = computed(() => chat.messages.length > 0)
const showEmpty = computed(() => !chat.loading && chat.messages.length === 0)

/** 压缩分割线位置：插在「第一条未被压缩的消息」前面（conv.compacted_before 之后的首条） */
const compactLineBeforeId = computed(() => {
  const cb = chat.conv?.compacted_before ?? 0
  if (cb <= 0) return 0
  const first = chat.messages.find((m) => m.id > cb)
  return first?.id ?? 0
})

function distanceFromBottom(): number {
  const el = scroller.value
  if (!el) return 0
  return el.scrollHeight - el.scrollTop - el.clientHeight
}

function onScroll(): void {
  atBottom.value = distanceFromBottom() < STICK_THRESHOLD
  // 滚近顶部：自动向上翻页（保持滚动位置不跳——记下加载前 scrollHeight，
  // prepend 后把 scrollTop 补上新增的高度差）
  const el = scroller.value
  if (!el || el.scrollTop > 120 || !chat.hasMore || chat.loadingOlder) return
  const prevH = el.scrollHeight
  void chat.loadOlder().then((loaded) => {
    if (!loaded) return
    void nextTick(() => {
      el.scrollTop += el.scrollHeight - prevH
    })
  })
}

function scrollToBottom(smooth = false): void {
  const el = scroller.value
  if (!el) return
  el.scrollTo({ top: el.scrollHeight, behavior: smooth ? 'smooth' : 'auto' })
  atBottom.value = true
}

// 内容变化（delta 每帧 flush 一次会自增 revision）→ 贴底才跟随
watch(
  () => chat.revision,
  async () => {
    if (!atBottom.value) return
    await nextTick()
    scrollToBottom()
  },
)

// 切换会话：直接跳到底部
watch(
  () => chat.conv?.id,
  async () => {
    atBottom.value = true
    await nextTick()
    scrollToBottom()
  },
)

onMounted(() => {
  scrollToBottom()
})
</script>

<template>
  <div ref="scroller" class="msglist" @scroll.passive="onScroll">
    <div v-if="chat.loading && !hasMessages" class="msglist__loading">
      <span class="spinner" /> 正在加载会话…
    </div>

    <div v-else-if="showEmpty" class="msglist__empty">
      <template v-if="chat.conv">
        <h2>{{ chat.conv.title || '新对话' }}</h2>
        <p>在下面的输入框里描述你的问题，Enter 发送、Ctrl+Enter 换行。</p>
      </template>
      <template v-else>
        <h2>还没有打开的会话</h2>
        <p>左侧「+ 新对话」可以开一个新的，或者从历史会话里挑一个继续。</p>
        <p style="margin-top: 12px">
          <button
            type="button"
            class="btn btn--primary"
            :disabled="convs.creating"
            @click="convs.create()"
          >
            新建对话
          </button>
        </p>
      </template>
    </div>

    <div v-else class="msglist__inner">
      <div v-if="chat.loadingOlder" class="msglist__older">
        <span class="spinner" /> 加载更早的消息…
      </div>
      <div v-else-if="chat.hasMore" class="msglist__older msglist__older--hint">往上滚加载更早的消息</div>
      <template v-for="m in chat.messages" :key="m.id">
        <div v-if="compactLineBeforeId === m.id" class="msglist__compact-line">
          —— 以上内容已压缩为摘要 · 只读 ——
        </div>
        <MessageItem :message="m" />
      </template>
    </div>

    <div v-if="hasMessages" class="msglist__jump">
      <button v-show="!atBottom" type="button" @click="scrollToBottom(true)">
        <svg width="11" height="11" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-arrow-down" />
        </svg>
        回到底部
      </button>
    </div>
  </div>
</template>
