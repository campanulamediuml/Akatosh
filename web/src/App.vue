<script setup lang="ts">
/**
 * 应用外壳：左右两栏 + 顶部条 + 设置弹窗 + toast。
 *
 * 启动顺序（契约要求「先建立 SSE 连接、再拉快照」，反了会在两步之间丢事件）：
 *   1. stream.connect()          唯一的 SSE 长连接
 *   2. settings.load()           供应商 / 模型 / 默认值
 *   3. conversations.load()      会话列表 → activeId → watcher 拉 state
 *
 * 真相永远在后端：这里不做任何乐观更新，只负责把 store 的状态摆到界面上。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import IconSprite from '@/components/IconSprite.vue'
import Sidebar from '@/components/Sidebar.vue'
import MessageList from '@/components/MessageList.vue'
import Composer from '@/components/Composer.vue'
import SettingsDialog from '@/components/SettingsDialog.vue'
import TokenDialog from '@/components/TokenDialog.vue'
import ToastHost from '@/components/ToastHost.vue'
import { setUnauthorizedHandler, api, toError } from '@/api/client'
import { toast } from '@/stores/toast'
import { useChatStore } from '@/stores/chat'
import { useConversationsStore } from '@/stores/conversations'
import { useSettingsStore } from '@/stores/settings'
import { close as closeStream, connect, ensureConnected, waitOpen } from '@/stores/stream'
import { EFFORT_LABELS } from '@/api/types'

const chat = useChatStore()
const convs = useConversationsStore()
const settings = useSettingsStore()

/* ---------------- 主题（暗色默认，记忆到 localStorage） ---------------- */

const THEME_KEY = 'ai-assistant-theme'
type Theme = 'dark' | 'light'

function readTheme(): Theme {
  try {
    const v = localStorage.getItem(THEME_KEY)
    if (v === 'light' || v === 'dark') return v
  } catch {
    /* 隐私模式下 localStorage 可能抛错，忽略 */
  }
  return 'dark'
}

const theme = ref<Theme>(readTheme())

watch(theme, (t) => {
  document.documentElement.setAttribute('data-theme', t)
  try {
    localStorage.setItem(THEME_KEY, t)
  } catch {
    /* 忽略 */
  }
})

function toggleTheme(): void {
  theme.value = theme.value === 'dark' ? 'light' : 'dark'
}

/* ---------------- 手动压缩上下文 ---------------- */

/** 压缩中（conv.compacting 由 SSE 推）或生成中禁用；结果 notice 会自己弹 toast */
const compacting = computed(() => chat.conv?.compacting ?? false)

async function compactNow(): Promise<void> {
  const id = chat.conv?.id
  if (!id || compacting.value || chat.isGenerating) return
  try {
    await api.compactConversation(id)
    toast.info('开始压缩上下文…')
  } catch (err) {
    toast.error(`压缩失败：${toError(err).message}`)
  }
}

/* ---------------- 布局 ---------------- */

const SIDEBAR_KEY = 'ai-assistant-sidebar'
/** 窄屏阈值，必须与 main.css 里 @media (max-width: 860px) 保持一致 */
const NARROW = '(max-width: 860px)'

function isNarrow(): boolean {
  return window.matchMedia(NARROW).matches
}

/**
 * 侧栏初始开合。
 *
 * 之前是写死的 ref(true)：手机每次刷新都弹回展开态，而展开时侧栏固定占 260px，
 * 390px 宽的屏上对话区只剩 130px，等于每次进页面都得先手动收一次。
 * 现在：记住用户的选择；没记录过就按屏宽定（窄屏收起、宽屏展开）。
 */
function readSidebarOpen(): boolean {
  try {
    const v = localStorage.getItem(SIDEBAR_KEY)
    if (v === '1') return true
    if (v === '0') return false
  } catch {
    /* 隐私模式下 localStorage 可能抛错，忽略 */
  }
  return !isNarrow()
}

const sidebarOpen = ref(readSidebarOpen())
const settingsOpen = ref(false)

watch(sidebarOpen, (v) => {
  try {
    localStorage.setItem(SIDEBAR_KEY, v ? '1' : '0')
  } catch {
    /* 忽略 */
  }
})

/**
 * 手机上选中会话后自动收起抽屉 —— 选完就该看对话，抽屉继续盖着没有意义。
 * 桌面端不受影响（宽屏下侧栏不是抽屉，收不收是用户自己的事）。
 */
watch(
  () => convs.activeId,
  (id) => {
    if (id !== null && isNarrow()) sidebarOpen.value = false
  },
)

/* ---------------- 访问令牌 ---------------- */

/**
 * 后端用 -token 启动、而这个浏览器没有（或给错）令牌时，任何 /api/* 都会 401。
 * client.ts / stream store 会通过这个钩子报到外壳来，弹输入框。
 *
 * 正常情况下根本不会触发：手机第一次带 ?token= 打开就换了 cookie。
 */
const tokenOpen = ref(false)

/* ---------------- 软键盘 ---------------- */

/**
 * iOS 弹键盘时只缩 visualViewport，布局视口不变，所以 height:100dvh 的壳子
 * 不会变矮，固定在底部的输入框就留在键盘底下。这里把「被键盘盖住的高度」算出来
 * 写进 --kb-h，CSS 里 .app 的高度再减掉它。
 *
 * 为什么要有 80px 门槛：地址栏收起/展开会让 visualViewport 变化几十像素，
 * 那不是键盘；只有聚焦在输入框里、且缺口明显大于一个地址栏时才当作键盘。
 */
function syncKeyboard(): void {
  const vv = window.visualViewport
  if (!vv) return
  const el = document.activeElement
  const typing = !!el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA')
  const covered = window.innerHeight - vv.height - vv.offsetTop
  const kb = typing && covered > 80 ? Math.round(covered) : 0
  document.documentElement.style.setProperty('--kb-h', `${kb}px`)
}

/* ---------------- 会话切换 ---------------- */

// activeId 一变就去拉那个会话的 state（openConversation 内部走闸门 + 快照）
watch(
  () => convs.activeId,
  async (id) => {
    if (id === null) {
      chat.reset()
      return
    }
    if (chat.conv?.id === id) return
    // 必须先确保 SSE 已经连上再拉快照：顺序反了，两步之间发布的事件会永久丢失
    await waitOpen()
    if (convs.activeId !== id) return // 等待期间用户又切走了
    void chat.openConversation(id)
  },
)

const convTitle = computed(() => chat.conv?.title || '未选择会话')

/* ---------------- 顶栏重命名（侧栏也有，但那个要悬停才看得见） ---------------- */

const renaming = ref(false)
const renameDraft = ref('')
const renameInput = ref<HTMLInputElement | null>(null)

function startRename(): void {
  if (!chat.conv) return
  renameDraft.value = chat.conv.title
  renaming.value = true
  void nextTick(() => {
    renameInput.value?.focus()
    renameInput.value?.select()
  })
}

function cancelRename(): void {
  renaming.value = false
}

/** 保存自定义标题。AI 起的标题不会覆盖它（后端只在标题还是本地截断值时才替换）。 */
async function commitRename(): Promise<void> {
  if (!renaming.value) return
  renaming.value = false
  const title = renameDraft.value.trim()
  if (!title || !chat.conv || title === chat.conv.title) return
  await chat.patchConv({ title })
}

const convSub = computed(() => {
  const c = chat.conv
  if (!c) return ''
  const bits: string[] = []
  // 会话绑定的是「哪家供应商 + 哪个模型」，两个都显示，同名模型才分得清
  if (c.provider_name) bits.push(c.provider_name)
  if (c.model_id) bits.push(c.model_id)
  bits.push(`${c.message_count}条消息`)
  if (c.effort) bits.push(`强度${EFFORT_LABELS[c.effort] ?? c.effort}`)
  // 窄屏下每一寸都贵：分隔用紧凑的「·」，不带空格
  return bits.join('·')
})

/* ---------------- 生命周期 ---------------- */

function onVisibility(): void {
  if (document.visibilityState === 'visible') ensureConnected()
}

function onOnline(): void {
  ensureConnected()
}

onMounted(async () => {
  // 1. 先建流
  connect()
  document.addEventListener('visibilitychange', onVisibility)
  window.addEventListener('online', onOnline)

  // 401 上报：任何接口（含 SSE）被拒都弹「输入访问令牌」
  setUnauthorizedHandler(() => {
    tokenOpen.value = true
  })

  // 软键盘：visualViewport 的尺寸变化是键盘弹起/收起的唯一可靠信号，
  // 再配合 focusin/focusout 先把状态算一次（resize 还没来时不至于卡着上一个高度）。
  const vv = window.visualViewport
  vv?.addEventListener('resize', syncKeyboard)
  vv?.addEventListener('scroll', syncKeyboard)
  document.addEventListener('focusin', syncKeyboard)
  document.addEventListener('focusout', syncKeyboard)
  syncKeyboard()

  // 2. 设置（一家供应商都没配就自动弹出设置弹窗）
  // 这里必须各自兜住异常：开着 -token 又没带令牌时，下面每个请求都会 401。
  // 不兜的话第一个 await 就抛出，后面的会话列表永远不会加载，界面停在半路。
  try {
    await settings.load()
    // 已经因为 401 弹了「输入令牌」时不要再叠一个设置窗：
    // 那时 providers 必然是空的（压根没读到），弹设置只会让人以为要配供应商。
    if (!tokenOpen.value && settings.providers.length === 0) settingsOpen.value = true
  } catch {
    /* 401 已经由上面的钩子弹了输入框，这里不再重复提示 */
  }

  // 3. 会话列表（空列表会自动建一条），activeId 变了上面的 watcher 会拉 state
  try {
    await convs.load()
  } catch {
    /* 同上 */
  }
})

onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisibility)
  window.removeEventListener('online', onOnline)
  setUnauthorizedHandler(null)
  const vv = window.visualViewport
  vv?.removeEventListener('resize', syncKeyboard)
  vv?.removeEventListener('scroll', syncKeyboard)
  document.removeEventListener('focusin', syncKeyboard)
  document.removeEventListener('focusout', syncKeyboard)
  closeStream()
})
</script>

<template>
  <div class="app" :class="{ 'app--collapsed': !sidebarOpen }">
    <IconSprite />

    <Sidebar @toggle="sidebarOpen = !sidebarOpen" @open-settings="settingsOpen = true" />

    <!-- 窄屏抽屉的遮罩：点一下关掉侧栏。桌面端不显示（CSS 里 .app__scrim 默认 display:none） -->
    <div v-if="sidebarOpen" class="app__scrim" @click="sidebarOpen = false" />

    <div class="app__main">
      <header class="topbar">
        <button
          type="button"
          class="icon-btn"
          :title="sidebarOpen ? '折叠侧栏' : '展开侧栏'"
          @click="sidebarOpen = !sidebarOpen"
        >
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-sidebar" />
          </svg>
        </button>

        <div class="topbar__title">
          <div class="topbar__nameline">
            <input
              v-if="renaming"
              ref="renameInput"
              v-model="renameDraft"
              class="input topbar__rename"
              maxlength="60"
              spellcheck="false"
              @keydown.enter.prevent="commitRename"
              @keydown.esc.prevent="cancelRename"
              @blur="commitRename"
            />
            <template v-else>
              <span class="topbar__name" :title="convTitle">{{ convTitle }}</span>
              <button
                v-if="chat.conv"
                type="button"
                class="icon-btn topbar__pen"
                title="重命名对话"
                @click="startRename"
              >
                <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                  <use href="#i-pencil" />
                </svg>
              </button>
            </template>
          </div>
          <span v-if="convSub" class="topbar__sub">{{ convSub }}</span>
        </div>

        <div class="topbar__actions">
          <!-- session 窗口只放会话级操作：压缩当前上下文（全局设置在侧栏「AI 助手」右边） -->
          <button
            v-if="chat.conv"
            type="button"
            class="icon-btn"
            :disabled="compacting || chat.isGenerating"
            :title="compacting ? '正在压缩上下文…' : '压缩当前上下文（较早的消息压为摘要并冻结只读）'"
            @click="compactNow"
          >
            <span v-if="compacting" class="spinner" />
            <svg v-else width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-compress" />
            </svg>
          </button>
          <button
            type="button"
            class="icon-btn"
            :title="theme === 'dark' ? '切换到亮色主题' : '切换到暗色主题'"
            @click="toggleTheme"
          >
            <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
              <use :href="theme === 'dark' ? '#i-sun' : '#i-moon'" />
            </svg>
          </button>
        </div>
      </header>

      <div class="workspace">
        <MessageList />
        <Composer @open-settings="settingsOpen = true" />
      </div>
    </div>

    <SettingsDialog :open="settingsOpen" @close="settingsOpen = false" />
    <TokenDialog :open="tokenOpen" @close="tokenOpen = false" />
    <ToastHost />
  </div>
</template>

<style scoped>
/* .app__main 是 flex 列容器，这里保证工作区能吃掉剩余高度（页面本身不滚动） */
.workspace {
  flex: 1 1 auto;
  min-height: 0;
}

.topbar__nameline {
  display: flex;
  align-items: center;
  gap: 4px;
  min-width: 0;
}

/* 铅笔按钮平时淡一点，鼠标进到标题区域才亮起来，别抢视线 */
.topbar__pen {
  opacity: 0.35;
  transition: opacity 0.15s;
}
.topbar__title:hover .topbar__pen {
  opacity: 1;
}

.topbar__rename {
  width: min(420px, 46vw);
  height: 26px;
  font-size: 13px;
  font-weight: 600;
}
</style>
