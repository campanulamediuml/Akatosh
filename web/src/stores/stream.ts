/**
 * 唯一的 SSE 长连接管家（模块级单例，整个应用只连一次）。
 *
 * 职责：
 * - 用 streamSse 维持 `GET /api/stream` 这一条流，fetch + reader 解析在 api/sse.ts；
 * - 收到事件后交给 chat store（高水位判断 + 闸门 + 分发）——事件本身不带任何
 *   请求上下文，全部靠 conv_id / message_id 区分；
 * - 断开按指数退避自动重连（1s→2s→4s→8s，上限 15s）；
 * - 静默死连接看门狗：锁屏/切网造成的 TCP 黑洞不会触发 fetch 报错，
 *   靠「45s 没收到任何字节（含 15s 心跳）」发现并主动重连；
 * - **重连成功即全量重同步**：会话列表 + 当前会话 state。因为真相在后端，
 *   重同步能纠正一切偏差（丢事件、丢字节、切标签页期间的堆积）。
 * - 发送回执确认（noteMessageSent）：POST ack 后 3s 没收到事件就立即重同步，
 *   不等看门狗。
 *
 * 连接顺序很重要：必须先建立 SSE，再拉快照；反了会在两步之间丢事件。
 */

import { reactive } from 'vue'
import { api, authHeaders, notifyUnauthorized, toError } from '@/api/client'
import { streamSse, DEFAULT_STALL_TIMEOUT_MS } from '@/api/sse'
import type { HelloPayload } from '@/api/types'
import { useChatStore, flushDeltaBuffer } from './chat'
import { useConversationsStore } from './conversations'
import { toast } from './toast'

export type StreamStatus = 'idle' | 'connecting' | 'open' | 'reconnecting'

export interface StreamConnectionState {
  status: StreamStatus
  /** hello 事件里的 client_id */
  clientId: string
  /** 后端时间（用于诊断时钟偏差） */
  serverTime: number
  /** 本轮重连已经试了几次 */
  attempts: number
  /** 上次断开的原因（给人看的） */
  lastError: string
  connectedAt: number
  /** 收到的事件总数（诊断用） */
  events: number
}

export const streamState = reactive<StreamConnectionState>({
  status: 'idle',
  clientId: '',
  serverTime: 0,
  attempts: 0,
  lastError: '',
  connectedAt: 0,
  events: 0,
})

/** 指数退避：1s → 2s → 4s → 8s，上限 15s */
const BACKOFF_MS = [1000, 2000, 4000, 8000, 15000]

let controller: AbortController | null = null
let retryTimer: number | null = null
let attempt = 0
let everConnected = false
let started = false
let closedByUs = false

/* ------------------------------------------------------------------ */
/* 静默死连接看门狗                                                      */
/*                                                                      */
/* 手机锁屏 / 切网（Wi-Fi↔蜂窝）/ 前置代理掉线时，TCP 连接会被「黑洞」：   */
/* fetch 不报错、也不再收到任何字节，连接表面上还开着。消息事件全走这条   */
/* 流，于是出现「消息发出去了（POST 走新连接成功了）但界面不更新，要刷新」。 */
/*                                                                      */
/* 后端每 15s 发一次心跳注释（internal/httpapi/stream.go），所以「开着   */
/* 的连接 45s 没有任何字节」就是死了 —— 主动掐掉，走既有的 onClose →      */
/* 退避重连 → 全量重同步路径。                                           */
/* ------------------------------------------------------------------ */

/** 最后一次从流上收到字节（含心跳）的时间 */
let lastRxAt = 0
let watchdogTimer: number | null = null

const WATCHDOG_INTERVAL_MS = 5_000

function markRx(): void {
  lastRxAt = Date.now()
}

/** 连接在表面上是否还活着（开着且没超时）。供可见性恢复时判断用 */
function connectionStale(): boolean {
  return controller !== null && Date.now() - lastRxAt > DEFAULT_STALL_TIMEOUT_MS
}

function startWatchdog(): void {
  if (watchdogTimer !== null) return
  watchdogTimer = window.setInterval(() => {
    if (!connectionStale()) return
    streamState.lastError = '连接静默超时（超过 45s 无心跳），主动重连'
    // abort 会触发 reader 出错 → onClose → scheduleReconnect，复用既有重连路径
    controller?.abort()
  }, WATCHDOG_INTERVAL_MS)
}

function stopWatchdog(): void {
  if (watchdogTimer !== null) {
    clearInterval(watchdogTimer)
    watchdogTimer = null
  }
}

/* ------------------------------------------------------------------ */
/* 连接生命周期                                                        */
/* ------------------------------------------------------------------ */

/** 建立唯一的那条流；重复调用是安全的（已经在连就直接返回） */
export function connect(): void {
  if (started) {
    ensureConnected()
    return
  }
  started = true
  closedByUs = false
  startWatchdog()
  openStream()
}

/** 主动关闭（页面卸载时用），不会触发自动重连 */
export function close(): void {
  closedByUs = true
  started = false
  attempt = 0
  clearRetry()
  stopWatchdog()
  flushPendingDeltas()
  if (controller) {
    controller.abort()
    controller = null
  }
  streamState.status = 'idle'
}

/** 断线了但重连定时器也没了（例如切回标签页时），补一次连接 */
export function ensureConnected(): void {
  if (!started) {
    connect()
    return
  }
  // 连接还「开着」但已经静默超时（锁屏/切网的 TCP 黑洞）：主动掐掉，
  // onClose 会接管重连。不加这个分支的话，卡死的连接会让这里永远空转。
  if (connectionStale()) {
    controller?.abort()
    return
  }
  if (controller === null && retryTimer === null) openStream()
}

export function isOpen(): boolean {
  return streamState.status === 'open'
}

/**
 * 等这条流真正连上再继续。
 *
 * 为什么需要：契约要求「先建立 SSE、再拉快照」，否则两步之间发布的事件会永久丢失。
 * 启动时 connect() 与随后的 load() 是并发的，虽然本地几乎总是流先就绪，
 * 但不能靠「几乎总是」——拉会话快照前显式等一下。
 *
 * 超时后仍然放行：流真的连不上时，重连成功后的 resync 会把状态补齐。
 */
export function waitOpen(timeoutMs = 3000): Promise<boolean> {
  if (streamState.status === 'open') return Promise.resolve(true)
  if (!started) connect()
  return new Promise((resolve) => {
    const t0 = Date.now()
    const timer = window.setInterval(() => {
      if (streamState.status === 'open') {
        window.clearInterval(timer)
        resolve(true)
        return
      }
      if (Date.now() - t0 > timeoutMs) {
        window.clearInterval(timer)
        resolve(false)
      }
    }, 20)
  })
}

/** 缓冲区里的尾巴先落地：关流/切会话都不能丢最后几个 token */
export function flushPendingDeltas(): void {
  flushDeltaBuffer()
}

function clearRetry(): void {
  if (retryTimer !== null) {
    clearTimeout(retryTimer)
    retryTimer = null
  }
}

function openStream(): void {
  streamState.status = everConnected || attempt > 0 ? 'reconnecting' : 'connecting'
  // 每条新连接都给完整的静默判定窗口（慢网络下连接建立本身可能要好几秒）
  markRx()

  controller = streamSse(
    api.streamUrl(),
    // 令牌：常规情况下靠 cookie 就够了，这里补 header 是为了 cookie 被禁用的环境
    { method: 'GET', headers: authHeaders() },
    {
      onOpen() {
        markRx()
        const reconnected = everConnected
        everConnected = true
        attempt = 0
        streamState.status = 'open'
        streamState.attempts = 0
        streamState.lastError = ''
        streamState.connectedAt = Date.now()
        // 重连成功后做一次全量重同步（首次连接由 App 的启动流程负责）
        if (reconnected) void resync()
      },
      // 心跳注释块也算：任何字节都说明连接活着
      onChunk: markRx,
      onEvent: handleEvent,
      onError(err) {
        streamState.lastError = toError(err).message
        // SSE 走的是 fetch，错误不经过 client.request，401 要在这里单独上报
        notifyUnauthorized(err)
      },
      onClose() {
        controller = null
        if (closedByUs || !started) {
          streamState.status = 'idle'
          return
        }
        scheduleReconnect()
      },
    },
  )
}

function scheduleReconnect(): void {
  if (retryTimer !== null) return
  const delay = BACKOFF_MS[Math.min(attempt, BACKOFF_MS.length - 1)] ?? 15000
  attempt += 1
  streamState.status = 'reconnecting'
  streamState.attempts = attempt
  retryTimer = window.setTimeout(() => {
    retryTimer = null
    if (!started || closedByUs) return
    openStream()
  }, delay)
}

/* ------------------------------------------------------------------ */
/* 事件分发                                                            */
/* ------------------------------------------------------------------ */

function handleEvent(name: string, data: unknown): void {
  const chat = useChatStore()
  streamState.events += 1

  // hello 是连接元信息，不受高水位约束（重连时它的 seq 可能小于 lastSeq）
  if (name === 'hello') {
    const hello = (data ?? {}) as Partial<HelloPayload>
    if (typeof hello.client_id === 'string') streamState.clientId = hello.client_id
    if (typeof hello.server_time === 'number') streamState.serverTime = hello.server_time
    if (chat.noteHello(hello as HelloPayload)) {
      // 后端重启过：seq 归零，本地高水位已经跟着回退，这里必须重同步一次
      toast.info('后端已重启，正在重新同步')
      void resync()
    }
    return
  }

  chat.onEvent(name, data)
}

/**
 * 全量重同步：会话列表 + 当前会话 state。
 * 两步都在同一个闸门窗口里完成 —— 窗口期内到达的 SSE 事件会被排队，
 * 快照落地后只回放 `seq > state.seq` 的那些，因此既不丢也不重。
 */
export async function resync(): Promise<void> {
  const chat = useChatStore()
  const convs = useConversationsStore()

  chat.beginSnapshot()
  try {
    await convs.refresh()
    const id = chat.currentConvId()
    if (id === null) return
    try {
      await chat.fetchState(id)
    } catch (err) {
      streamState.lastError = toError(err).message
    }
  } finally {
    chat.endSnapshot()
  }
}

/**
 * 发送成功后的投递确认（由 Composer 在 POST ack 之后调用）。
 *
 * 契约是「POST 只回 ack，消息本体等 SSE 推」。推送链路上一切延迟都是
 * 秒级以内的；ack 后 3s 还没收到任何事件，说明长连接已经静默死了
 * （例如发送瞬间正好切网），直接全量重同步把消息拉下来，
 * 而不是让用户盯着空界面等到 45s 看门狗触发。
 */
export function noteMessageSent(): void {
  const eventsAtSend = streamState.events
  window.setTimeout(() => {
    if (streamState.events === eventsAtSend) void resync()
  }, 3000)
}
