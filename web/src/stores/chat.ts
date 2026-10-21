/**
 * 当前会话的消息、实时事件与上行命令。
 *
 * 三条硬约束（v2 契约）：
 * 1. **不做乐观更新**：发送后不往列表里插任何东西，等 `message` 事件；
 * 2. **seq 高水位**：只由「应用快照」推进，事件不推进它（否则会把已收到但
 *    尚未应用的序号提前吃掉）；收到 seq <= lastSeq 的事件一律丢弃；
 * 3. **闸门 + 排队 + 回放**：任何会整体替换状态的操作（切换会话、断线重连后的
 *    重同步、deleted/truncated 触发的重新拉取）都必须先开闸，窗口期内到达的
 *    事件排进 pending 队列，快照落地后再回放 `seq > state.seq` 的那些。
 *    没有这一层，快照请求飞行期间到达的增量会被快照覆盖且永不再来 → 永久丢字。
 *
 * 另外：delta 不直接写响应式状态，先进缓冲区，用 requestAnimationFrame 每帧
 * flush 一次（上游可达 100 事件/秒，逐个 delta 触发 markdown 重渲染会卡死）。
 */

import { defineStore } from 'pinia'
import { nextTick } from 'vue'
import { api, errorCode, toError } from '@/api/client'
import type {
  Conversation,
  ConvPayload,
  DeltaPayload,
  Generating,
  HelloPayload,
  Message,
  MessagePayload,
  NoticePayload,
  SendMessageInput,
  State,
  UpdateConversationInput,
} from '@/api/types'
import { payloadConvId, payloadSeq } from '@/api/types'
import { useConversationsStore } from './conversations'
import { useSettingsStore } from './settings'
import { toast } from './toast'

/* ================================================================== */
/* 模块级：整个应用共享一份的缓冲区与闸门（JS 单线程，不需要锁）          */
/* ================================================================== */

type Channel = 'reasoning' | 'content'
interface BufferedDelta {
  messageId: number
  channel: Channel
  text: string
}

interface PendingEvent {
  seq: number
  apply: () => void
}

/** rAF 节流缓冲：`${messageId}/${channel}` → 待应用增量 */
const deltaBuffer = new Map<string, BufferedDelta>()

/** 已经排好的 flush 句柄（rAF，或后台标签页里的 setTimeout 兜底） */
let flushHandle: { kind: 'raf' | 'timeout'; id: number } | null = null

/** 闸门深度：>0 表示有快照在飞，事件进 pending 而不直接应用 */
let gateDepth = 0

/** 闸门窗口期内到达的事件，按到达顺序回放 */
let pending: PendingEvent[] = []

/** 当前右侧展示（或正在打开）的会话 id：用来滤掉其它会话的 delta */
let activeConvId: number | null = null

/** 会话列表的合并刷新定时器 */
let listRefreshTimer: number | null = null

/**
 * 会话纪元：每次「切换会话 / 清空右侧」都自增。
 * 拉 state 期间用户又切走了的话，那份迟到的快照必须丢掉，
 * 否则它会把新会话的内容覆盖成旧会话的（本地 1ms 也可能乱序）。
 */
let convEpoch = 0

function pageHidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden'
}

function cancelScheduledFlush(): void {
  if (!flushHandle) return
  if (flushHandle.kind === 'raf') cancelAnimationFrame(flushHandle.id)
  else clearTimeout(flushHandle.id)
  flushHandle = null
}

function scheduleFlush(): void {
  if (flushHandle) return
  const run = (): void => {
    flushHandle = null
    flushDeltaBuffer()
  }
  // 后台标签页里 rAF 不触发，用 setTimeout 兜底，别让缓冲区无限涨
  if (typeof requestAnimationFrame === 'function' && !pageHidden()) {
    flushHandle = { kind: 'raf', id: requestAnimationFrame(run) }
  } else {
    flushHandle = { kind: 'timeout', id: window.setTimeout(run, 50) }
  }
}

/** 把缓冲区里的增量一次性写进响应式状态（每帧最多一次） */
export function flushDeltaBuffer(): void {
  cancelScheduledFlush()
  if (deltaBuffer.size === 0) return
  const batch: BufferedDelta[] = []
  for (const item of deltaBuffer.values()) batch.push(item)
  deltaBuffer.clear()
  useChatStore().applyDeltas(batch)
}

/**
 * 丢掉还没应用的增量。
 * 只在「即将应用一份更晚的快照」时调用：快照取的是后端内存实时值，
 * 一定包含这些增量；反过来如果不丢，切换会话后旧会话的 delta 会污染新会话。
 */
function discardDeltaBuffer(): void {
  cancelScheduledFlush()
  deltaBuffer.clear()
}

function queueDelta(messageId: number, channel: Channel, text: string): void {
  if (!text) return
  const key = `${messageId}/${channel}`
  const cur = deltaBuffer.get(key)
  if (cur) cur.text += text
  else deltaBuffer.set(key, { messageId, channel, text })
  scheduleFlush()
}

/** message 事件会改 message_count / 标题，合并成一次列表刷新 */
function scheduleListRefresh(): void {
  if (listRefreshTimer !== null) return
  listRefreshTimer = window.setTimeout(() => {
    listRefreshTimer = null
    if (gateDepth > 0) return // 闸门窗口里的重同步本来就会拉列表
    void useConversationsStore().refresh()
  }, 600)
}

function cancelListRefresh(): void {
  if (listRefreshTimer !== null) {
    clearTimeout(listRefreshTimer)
    listRefreshTimer = null
  }
}

function showNotice(p: NoticePayload): void {
  const text = typeof p?.text === 'string' ? p.text : ''
  if (!text) return
  if (p.level === 'error') toast.error(text)
  else toast.info(text)
}

/* ================================================================== */
/* Store                                                              */
/* ================================================================== */

export type SendFailure = 'busy' | 'empty' | 'no_conv' | 'error'

export interface SendResult {
  ok: boolean
  reason?: SendFailure
  message?: string
}

interface ChatState {
  conv: Conversation | null
  messages: Message[]
  generating: Generating | null
  /** 高水位：只由 applyState（快照）推进 */
  lastSeq: number
  loading: boolean
  /** 内容版本号：每次变化自增，消息区据此做「贴底才跟随」的自动滚动 */
  revision: number
  /**
   * 「请滚到底部」请求序号：发送成功时 +1。
   * 和 revision 的区别：revision 只在贴底时跟随（读历史时不能抢滚动条），
   * 而这个是用户主动发送后的强制回底 —— 从搜索跳转过来/翻完历史再发消息，
   * 都应该立刻看到刚发的那条。
   */
  scrollRequest: number
  /** 是否还有更早的消息可向上翻（state 首屏只带最近若干条） */
  hasMore: boolean
  /** 正在向上翻页加载更早消息 */
  loadingOlder: boolean
  /**
   * 生成中的联网搜索状态：messageID → 已经发生的搜索动作（「正在搜索：xxx」…）。
   * 这是内存态：回答落地后被清空（持久展示由消息的 search_log 承担）；
   * 刷新/切会话后自然丢失，可接受 —— 搜索过程本来就只持续几秒。
   */
  searchStatus: Record<number, string[]>
  /**
   * 生成中工具调用的 arguments 流式累积（模型正在写的代码/参数的 JSON 原文）。
   * messageID → 累积文本。同样是内存态，落地后由 search_log 接管展示。
   */
  toolArgs: Record<number, string>
}

export const useChatStore = defineStore('chat', {
  state: (): ChatState => ({
    conv: null,
    messages: [],
    generating: null,
    lastSeq: 0,
    loading: false,
    revision: 0,
    scrollRequest: 0,
    hasMore: false,
    loadingOlder: false,
    searchStatus: {},
    toolArgs: {},
  }),

  getters: {
    /** 正在流式输出的那条消息（判断「生成中」最可靠的依据） */
    streamingMessage(state): Message | null {
      return state.messages.find((m) => m.status === 'streaming') ?? null
    },

    /** 当前会话是否正在生成：Composer 据此把发送按钮换成停止按钮 */
    isGenerating(): boolean {
      return this.streamingMessage !== null || this.generating !== null
    },

    messageById(state): (id: number) => Message | null {
      return (id: number) => state.messages.find((m) => m.id === id) ?? null
    },

    /**
     * 会话上下文占用：{ used, total }。
     * - total 来自会话当前模型的 context_len（没声明则为 0，界面不显示）；
     * - used 取最近一条完成的助手消息的 prompt_tokens（上游实测，含 reasoning 回传）——
     *   每轮都会带上全部历史，它就是「当前历史有多大」的准确值。
     *   编辑正文/CoT 后下一次发送时，那轮 prompt 自然就是修改后的历史，随之更新。
     */
    contextUsage(): { used: number; total: number } | null {
      const conv = this.conv
      if (!conv) return null
      const m = useSettingsStore().modelIn(conv.provider_id, conv.model_id)
      const total = m?.context_len ?? 0
      if (total <= 0) return null
      const last = [...this.messages]
        .reverse()
        .find((x) => x.role === 'assistant' && x.status !== 'streaming' && (x.usage?.prompt_tokens ?? 0) > 0)
      return { used: last?.usage.prompt_tokens ?? 0, total }
    },
  },

  actions: {
    /* ------------------------------------------------------------ */
    /* 高水位 / 闸门                                                 */
    /* ------------------------------------------------------------ */

    /**
     * 事件入口。**所有** SSE 事件都必须经过这里。
     * - 先做高水位判断：seq <= lastSeq 说明快照已经包含它 → 丢弃；
     * - 闸门开着就排队，等快照落地后回放。
     */
    onEvent(name: string, data: unknown): void {
      const seq = payloadSeq(data)
      if (seq > 0 && seq <= this.lastSeq) return
      const apply = (): void => this.route(name, data)
      if (gateDepth > 0) {
        pending.push({ seq, apply })
        return
      }
      apply()
    },

    route(name: string, data: unknown): void {
      switch (name) {
        case 'delta':
          this.applyDeltaEvent(data as DeltaPayload)
          break
        case 'message':
          this.applyMessageEvent(data as MessagePayload)
          break
        case 'conv':
          this.applyConvEvent(data as ConvPayload)
          break
        case 'notice':
          showNotice(data as NoticePayload)
          break
        default:
          break
      }
    },

    /**
     * 开闸：进入「即将整体替换状态」的窗口。
     * - 旧队列作废：这些事件代表的变更会被即将到达的快照覆盖；
     * - 丢弃 delta 缓冲区：同理，快照包含它们，留着只会污染新状态。
     *
     * 用深度计数而不是布尔量：切换会话和断线重同步有可能重叠，
     * 谁最先关闸都不能提前把闸门打开（否则后来那段事件会被漏掉）。
     */
    beginSnapshot(): void {
      if (gateDepth === 0) {
        pending = []
        discardDeltaBuffer()
      }
      gateDepth += 1
    },

    /** 关闸并回放：只回放快照还没包含的（seq > lastSeq）事件 */
    endSnapshot(): void {
      if (gateDepth > 0) gateDepth -= 1
      if (gateDepth > 0) return // 还有快照在飞，等最后一个关闸时统一回放
      const queue = pending
      pending = []
      for (const item of queue) {
        if (item.seq > this.lastSeq) item.apply()
      }
    },

    /**
     * hello 事件不走高水位（它的 seq 可能小于 lastSeq）。
     * 返回 true 表示发现后端重启过（全局 seq 归零，本地高水位必须跟着回退，
     * 否则后面的事件会被全部误判为过期而丢弃）。
     */
    noteHello(payload: HelloPayload | null | undefined): boolean {
      const seq = typeof payload?.seq === 'number' && Number.isFinite(payload.seq) ? payload.seq : 0
      if (seq > 0 && seq < this.lastSeq) {
        this.lastSeq = seq
        return true
      }
      return false
    },

    /** 当前右侧展示（或正在打开）的会话 id */
    currentConvId(): number | null {
      return this.conv?.id ?? activeConvId
    },

    /* ------------------------------------------------------------ */
    /* 快照                                                          */
    /* ------------------------------------------------------------ */

    /**
     * 拉一次会话 state（原子的整体替换，调用方负责开闸/关闸）。
     * epoch 用来丢弃「在飞行途中已经过时」的快照（用户又切走了）。
     */
    async fetchState(id: number, epoch?: number): Promise<void> {
      const token = epoch ?? convEpoch
      const st = await api.convState(id)
      if (token !== convEpoch) return // 期间切过会话，这份快照已经作废
      if (this.currentConvId() !== id) return
      this.applyState(st)
    },

    applyState(st: State | null | undefined): void {
      const conv = st?.conv ?? null
      const msgs = Array.isArray(st?.messages) ? [...st.messages] : []
      msgs.sort((a, b) => a.id - b.id)

      this.conv = conv
      this.messages = msgs
      // generating 非空时，那条消息的 reasoning/content 已经是后端内存里的实时值，
      // 直接渲染即可，随后到达的 delta 会继续往上追加。
      this.generating = st?.generating ?? null
      this.hasMore = st?.has_more === true
      this.loadingOlder = false
      if (typeof st?.seq === 'number' && Number.isFinite(st.seq)) this.lastSeq = st.seq
      // 快照是权威状态：搜索过程状态一并重建（生成中的消息由后续 delta 重新累积）
      this.searchStatus = {}
      this.toolArgs = {}

      if (conv) {
        activeConvId = conv.id
        // conv.generating 由后端保证权威（生成开始/结束都会广播 conv 事件）
        useConversationsStore().upsertConv(conv)
      }
      this.revision++
    },

    /** 切换会话：开闸 → 拉快照 → 关闸回放 */
    async openConversation(id: number): Promise<void> {
      convEpoch += 1
      const epoch = convEpoch
      activeConvId = id
      this.loading = true
      if (this.conv?.id !== id) {
        // 切到另一个会话：先把上一个会话的内容清掉，避免拉 state 期间显示错位的内容。
        // 这不是乐观更新（没有任何本地推测的内容），只是不等快照就先把旧内容撤下来。
        this.conv = null
        this.messages = []
        this.generating = null
        this.revision++
      }
      this.beginSnapshot()
      try {
        await this.fetchState(id, epoch)
        // 打开就算看过了：熄灭侧边栏的小红点
        void useConversationsStore().markRead(id)
      } catch (err) {
        if (epoch === convEpoch) this.handleStateError(err, id)
      } finally {
        this.loading = false
        this.endSnapshot()
      }
    },

    /** 静默重拉当前会话（deleted / truncated 事件，以及手动重同步） */
    async refreshState(): Promise<void> {
      const id = this.currentConvId()
      if (id === null) return
      activeConvId = id
      const epoch = convEpoch
      this.beginSnapshot()
      try {
        await this.fetchState(id, epoch)
      } catch (err) {
        if (epoch === convEpoch) this.handleStateError(err, id)
      } finally {
        this.endSnapshot()
      }
    },

    handleStateError(err: unknown, id: number): void {
      if (errorCode(err) === 'not_found') {
        useConversationsStore().handleConvDeleted(id)
        toast.error('这个会话已经不存在了')
        return
      }
      toast.error(`读取会话失败：${toError(err).message}`)
    },

    /** 右侧清空（当前会话被删掉时） */
    reset(): void {
      convEpoch += 1 // 让还在飞的快照作废
      activeConvId = null
      this.conv = null
      this.messages = []
      this.generating = null
      this.loading = false
      this.hasMore = false
      this.loadingOlder = false
      this.searchStatus = {}
      this.toolArgs = {}
      discardDeltaBuffer()
      cancelListRefresh()
      this.revision++
    },

    /**
     * 向上翻页：加载更早的消息并前插。
     * 调用方（MessageList）负责在加载前后保持滚动位置不跳。
     * 返回是否加载到了内容（false = 没有更早的了或在加载中）。
     */
    async loadOlder(): Promise<boolean> {
      const conv = this.conv
      if (!conv || this.loadingOlder || !this.hasMore || this.messages.length === 0) return false
      this.loadingOlder = true
      try {
        const before = this.messages[0].id
        const msgs = await api.listMessages(conv.id, before, 20)
        if (!Array.isArray(msgs)) return false
        this.messages = [...msgs, ...this.messages]
        if (msgs.length < 20) this.hasMore = false
        this.revision++
        return msgs.length > 0
      } catch (err) {
        toast.error(`加载更早的消息失败：${toError(err).message}`)
        return false
      } finally {
        this.loadingOlder = false
      }
    },

    /**
     * 跳到某条具体消息（搜索结果点击用）：
     * 先开会话，再按分页一直往上翻到目标消息出现，最后滚动过去并短暂高亮。
     * 已归档的冻结消息同样能跳（它们只是只读，不是不存在）。
     */
    /** 发送成功后调用：让消息区滚到底部并恢复贴底跟随 */
    requestScrollToBottom(): void {
      this.scrollRequest += 1
    },

    async openMessage(convId: number, messageId: number): Promise<boolean> {
      await this.openConversation(convId)
      // 目标可能远早于首屏：最多翻 100 页，防死循环
      for (let i = 0; i < 100; i++) {
        if (this.messages.some((m) => m.id === messageId)) break
        if (!this.hasMore) break
        const loaded = await this.loadOlder()
        if (!loaded) break
      }
      if (!this.messages.some((m) => m.id === messageId)) {
        toast.error('那条消息没能加载出来')
        return false
      }
      await nextTick()
      const el = document.querySelector(`[data-msg-id="${messageId}"]`)
      if (el) {
        // 必须 instant：smooth 滚动期间懒挂载会不断改变布局高度，动画永远追不上目标。
        // 先立即滚到位，挂载稳定后再补一次校正。
        // 每次滚动后手动派发一次 scroll：让 MessageList 重新计算 atBottom=false，
        // 否则 loadOlder 触发的「贴底跟随」会把我们刚滚到的位置顶回去。
        const sc = el.closest('.msglist')
        const stick = () => {
          // 对齐消息顶部而不是居中：长消息居中会把开头顶出屏幕，搜索结果没法读
          el.scrollIntoView({ block: 'start' })
          sc?.dispatchEvent(new Event('scroll'))
        }
        stick()
        window.setTimeout(stick, 300)
        el.classList.add('msg--flash')
        window.setTimeout(() => el.classList.remove('msg--flash'), 2400)
      }
      return true
    },

    /* ------------------------------------------------------------ */
    /* 事件处理                                                      */
    /* ------------------------------------------------------------ */

    applyDeltaEvent(p: DeltaPayload): void {
      if (!p || typeof p.message_id !== 'number') return
      const convId = payloadConvId(p)
      // 其它会话的 delta 直接丢：它不在屏幕上，切回去时快照会给完整内容
      if (convId !== null && activeConvId !== null && convId !== activeConvId) return
      if (!this.messages.some((m) => m.id === p.message_id)) return
      // 工具调用状态（联网搜索 / Python 执行）是低频事件，直接写响应式状态，不过 delta 缓冲区
      if (p.channel === 'tool') {
        const text = typeof p.text === 'string' ? p.text.trim() : ''
        if (text) {
          const list = this.searchStatus[p.message_id] ?? []
          this.searchStatus[p.message_id] = [...list, text]
          this.revision++
        }
        return
      }
      // 工具调用的 arguments 流式增量（模型正在写的代码/参数）：累积拼接
      if (p.channel === 'toolargs') {
        const text = typeof p.text === 'string' ? p.text : ''
        if (text) {
          this.toolArgs[p.message_id] = (this.toolArgs[p.message_id] ?? '') + text
          this.revision++
        }
        return
      }
      const channel: Channel = p.channel === 'reasoning' ? 'reasoning' : 'content'
      queueDelta(p.message_id, channel, typeof p.text === 'string' ? p.text : '')
    },

    /** 每帧一次的批量落地 */
    applyDeltas(batch: BufferedDelta[]): void {
      let touched = false
      for (const d of batch) {
        const m = this.messages.find((x) => x.id === d.messageId)
        if (!m) continue
        if (d.channel === 'reasoning') m.reasoning += d.text
        else m.content += d.text
        touched = true
      }
      if (touched) this.revision++
    },

    applyMessageEvent(p: MessagePayload): void {
      if (!p) return
      const convId = payloadConvId(p) ?? 0

      if (p.action === 'created' || p.action === 'updated') {
        const m = p.message
        if (!m || typeof m.id !== 'number') return
        if (convId === activeConvId) {
          this.upsertMessage(m)
          if (
            m.role === 'assistant' &&
            m.status !== 'streaming' &&
            this.generating?.message_id === m.id
          ) {
            this.generating = null
          }
          // 回答落地：搜索过程状态撤下，持久展示交给消息自带的 search_log
          if (m.role === 'assistant' && m.status !== 'streaming') {
            if (this.searchStatus[m.id]) delete this.searchStatus[m.id]
            if (this.toolArgs[m.id]) delete this.toolArgs[m.id]
          }
          // 回答落地时如果你正看着这个会话，就当你看过了 ——
          // 否则侧边栏会在你眼皮底下亮个红点，很莫名其妙。
          if (m.role === 'assistant' && m.status !== 'streaming') {
            void useConversationsStore().markRead(convId)
          }
        }
        // message_count / 自动标题可能变了；生成中的标记由 conv 事件负责，这里不推断
        scheduleListRefresh()
        return
      }

      // deleted / truncated：消息结构变了，事件里没有 message → 重新拉 state
      scheduleListRefresh()
      if (convId !== activeConvId) return
      void this.refreshState()
    },

    /** 用完整 message 覆盖，或按 id 顺序插入 */
    upsertMessage(m: Message): void {
      // 先落地缓冲区：这条事件带的是完整内容，不能让还没应用的 delta 再往它后面追加一遍
      flushDeltaBuffer()
      const idx = this.messages.findIndex((x) => x.id === m.id)
      if (idx >= 0) {
        this.messages.splice(idx, 1, m)
      } else {
        let at = this.messages.findIndex((x) => x.id > m.id)
        if (at < 0) at = this.messages.length
        this.messages.splice(at, 0, m)
      }
      this.revision++
    },

    applyConvEvent(p: ConvPayload): void {
      if (!p) return
      const convs = useConversationsStore()

      if (p.action === 'deleted') {
        convs.handleConvDeleted(p.conv_id)
        if (this.conv?.id === p.conv_id || activeConvId === p.conv_id) this.reset()
        return
      }

      const conv = p.conv
      if (!conv || typeof conv.id !== 'number') return
      // generating 现在是权威值：生成开始/结束各广播一次，列表脉冲直接跟着变
      convs.upsertConv(conv)
      if (this.conv?.id === conv.id) {
        // state.generating（带 started_at 等实时信息）由快照和 message 事件维护，
        // conv 事件不带它，所以这里只替换 conv 本身
        this.conv = conv
      }
    },

    /* ------------------------------------------------------------ */
    /* 上行命令（全部只返回 ack，内容等 SSE 推下来）                    */
    /* ------------------------------------------------------------ */

    /**
     * 发送消息。
     *
     * - 成功 → 什么都不做（等 `message` 事件把两条消息推下来，前端不做乐观更新）；
     * - 409 busy → 这条消息没有落库，调用方必须保留输入框里的文字和待发送图片；
     * - `attachmentIds`：上传拿到的附件 id（契约 §4）。允许 content 为空、只发图。
     *   这里只传 id，消息上的 `attachments` 由后端随 message 事件一起下发，
     *   前端**不**自己拼图片信息。
     */
    async send(text: string, attachmentIds?: number[], fileIds?: number[]): Promise<SendResult> {
      const conv = this.conv
      const content = text.trim()
      const ids = Array.isArray(attachmentIds)
        ? attachmentIds.filter((n) => typeof n === 'number' && Number.isFinite(n))
        : []
      const fids = Array.isArray(fileIds)
        ? fileIds.filter((n) => typeof n === 'number' && Number.isFinite(n))
        : []
      if (!conv) return { ok: false, reason: 'no_conv', message: '还没有打开会话' }
      if (!content && ids.length === 0 && fids.length === 0) return { ok: false, reason: 'empty' }

      const input: SendMessageInput = { content }
      // 契约里 attachment_ids / file_ids 都是可选的；没有就干脆不带这个字段
      if (ids.length > 0) input.attachment_ids = ids
      if (fids.length > 0) input.file_ids = fids

      try {
        await api.sendMessage(conv.id, input)
        return { ok: true }
      } catch (err) {
        if (errorCode(err) === 'busy') {
          const message = '上一条还在生成中，这条消息已丢弃'
          toast.info(message)
          return { ok: false, reason: 'busy', message }
        }
        if (errorCode(err) === 'compacting') {
          const message = '正在压缩上下文，请稍候再发'
          toast.info(message)
          return { ok: false, reason: 'busy', message }
        }
        const message = toError(err).message
        toast.error(`发送失败：${message}`)
        return { ok: false, reason: 'error', message }
      }
    },

    async stop(): Promise<void> {
      const conv = this.conv
      if (!conv) return
      try {
        const res = await api.stop(conv.id)
        if (res && res.stopped === false) toast.info('当前没有正在生成的回复')
      } catch (err) {
        toast.error(`停止失败：${toError(err).message}`)
      }
    },

    /**
     * 编辑正文。PATCH 的返回值就是权威内容，直接应用；
     * 后端同时也会广播 message(updated) 事件，重复应用是幂等的。
     */
    async editMessage(id: number, content: string): Promise<boolean> {
      try {
        const res = await api.patchMessage(id, { content })
        if (res?.message) {
          this.applyMessageEvent({
            seq: 0,
            conv_id: res.message.conv_id,
            action: 'updated',
            message_id: res.message.id,
            message: res.message,
          })
        }
        toast.success('已保存')
        return true
      } catch (err) {
        if (errorCode(err) === 'busy') toast.info('这条消息正在生成，暂时不能编辑')
        else toast.error(`保存失败：${toError(err).message}`)
        return false
      }
    },

    async restoreMessage(id: number): Promise<boolean> {
      try {
        const res = await api.restoreMessage(id)
        if (res?.message) {
          this.applyMessageEvent({
            seq: 0,
            conv_id: res.message.conv_id,
            action: 'updated',
            message_id: res.message.id,
            message: res.message,
          })
        }
        toast.success('已还原原文')
        return true
      } catch (err) {
        if (errorCode(err) === 'busy') toast.info('这条消息正在生成，暂时不能还原')
        else toast.error(`还原失败：${toError(err).message}`)
        return false
      }
    },

    /** 编辑 CoT：编辑后的 reasoning 会随历史回传给上游（DeepSeek 思考模式规则） */
    async editReasoning(id: number, reasoning: string): Promise<boolean> {
      try {
        const res = await api.patchMessage(id, { reasoning })
        if (res?.message) {
          this.applyMessageEvent({
            seq: 0,
            conv_id: res.message.conv_id,
            action: 'updated',
            message_id: res.message.id,
            message: res.message,
          })
        }
        toast.success('已保存')
        return true
      } catch (err) {
        if (errorCode(err) === 'busy') toast.info('这条消息正在生成，暂时不能编辑')
        else toast.error(`保存失败：${toError(err).message}`)
        return false
      }
    },

    /** 还原 CoT 到模型原文 */
    async restoreReasoning(id: number): Promise<boolean> {
      try {
        const res = await api.restoreMessageReasoning(id)
        if (res?.message) {
          this.applyMessageEvent({
            seq: 0,
            conv_id: res.message.conv_id,
            action: 'updated',
            message_id: res.message.id,
            message: res.message,
          })
        }
        toast.success('已还原原文')
        return true
      } catch (err) {
        if (errorCode(err) === 'busy') toast.info('这条消息正在生成，暂时不能还原')
        else toast.error(`还原失败：${toError(err).message}`)
        return false
      }
    },

    async deleteMessage(id: number): Promise<boolean> {
      try {
        await api.deleteMessage(id)
        // 消息结构变化由 message(deleted) 事件触发重新拉 state
        toast.success('已删除')
        return true
      } catch (err) {
        if (errorCode(err) === 'busy') toast.info('这条消息正在生成，暂时不能删除')
        else toast.error(`删除失败：${toError(err).message}`)
        return false
      }
    },

    async regenerate(id: number): Promise<boolean> {
      try {
        await api.regenerate(id)
        // 截断会通过 message(truncated) 事件通知，前端重新拉 state
        return true
      } catch (err) {
        if (errorCode(err) === 'busy') toast.info('该会话正在生成，无法重新生成')
        else toast.error(`重新生成失败：${toError(err).message}`)
        return false
      }
    },

    /** 会话属性（模型 / 思考强度 / 标题…）：PATCH 的返回值直接可用 */
    async patchConv(patch: UpdateConversationInput): Promise<boolean> {
      const conv = this.conv
      if (!conv) return false
      try {
        const res = await api.patchConversation(conv.id, patch)
        if (res?.conv) {
          // PATCH 响应是数据库行，generating 会滞后成 false；本地这份来自事件/快照，更可信
          const merged: Conversation =
            conv.generating && !res.conv.generating ? { ...res.conv, generating: true } : res.conv
          this.conv = merged
          useConversationsStore().upsertConv(res.conv, { preserveGenerating: true })
        }
        return true
      } catch (err) {
        toast.error(`保存会话设置失败：${toError(err).message}`)
        return false
      }
    },
  },
})
