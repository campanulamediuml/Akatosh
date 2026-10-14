/**
 * SSE-over-fetch 解析器。
 *
 * 为什么不用 EventSource：它不能 POST、不能带 body/header，而主链路是
 * `POST /api/conversations/{id}/messages` 且响应体就是事件流。
 *
 * 实现要点（对应 API.md §6.1）：
 * - fetch + response.body.getReader() + TextDecoder({ stream: true })
 * - 按空行切分事件块（\n\n / \r\n\r\n / \r\r），块内逐行解析 field: value
 * - 一个事件可以有多行 data，按 SSE 规范用 "\n" 拼接
 * - ":" 开头是注释（后端 15s 一次心跳 ": ping"），直接忽略
 * - data 解析失败只丢这一个事件，绝不让整个流崩掉
 * - 非 2xx 统一抛 EventError（busy 时带 run_id，由调用方改成接管流）
 */

import { EventError, NetworkError, toError } from './client'
import type { ApiErrorBody, ApiErrorCode } from './types'

export interface SseHandlers {
  onEvent: (event: string, data: any) => void
  onOpen?: () => void
  onClose?: () => void
  onError?: (err: Error) => void
  /**
   * 每收到一块原始字节就调一次（包括心跳注释块——它们不会变成事件）。
   * 用途：静默存活判定。移动设备锁屏/切网后 TCP 黑洞不会让 fetch 报错，
   * 「45s 没有任何字节（后端 15s 一次心跳）」是发现死连接的唯一办法。
   */
  onChunk?: () => void
}

/** 一个事件块解析出来的原始字段 */
interface RawEvent {
  event: string
  data: string
  id?: string
  retry?: number
}

/** 解析单个事件块；返回 null 表示整块都是注释/空 → 调用方应忽略 */
export function parseEventBlock(block: string): RawEvent | null {
  const lines = block.split('\n')
  let event = ''
  let id: string | undefined
  let retry: number | undefined
  const dataLines: string[] = []
  let sawField = false

  for (let raw of lines) {
    // 容忍 CRLF：\r\n 已经被切块处理过，这里再兜一次裸 \r
    if (raw.endsWith('\r')) raw = raw.slice(0, -1)
    if (raw === '') continue
    if (raw.charCodeAt(0) === 58 /* ':' */) continue // 注释 / 心跳

    const colon = raw.indexOf(':')
    let field: string
    let value: string
    if (colon === -1) {
      field = raw
      value = ''
    } else {
      field = raw.slice(0, colon)
      value = raw.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)
    }

    sawField = true
    switch (field) {
      case 'event':
        event = value
        break
      case 'data':
        dataLines.push(value)
        break
      case 'id':
        id = value
        break
      case 'retry': {
        const n = Number.parseInt(value, 10)
        if (!Number.isNaN(n)) retry = n
        break
      }
      default:
        // 未知字段按规范忽略
        break
    }
  }

  if (!sawField) return null
  // 规范：无 event 字段时默认事件名是 "message"
  return { event: event || 'message', data: dataLines.join('\n'), id, retry }
}

/** data 是单行 JSON（契约保证）；解析失败返回原始字符串而不是抛错 */
function decodeData(data: string): any {
  if (data === '') return ''
  try {
    return JSON.parse(data)
  } catch {
    console.warn('[sse] data 不是合法 JSON，按原始字符串处理：', data.slice(0, 200))
    return data
  }
}

async function readErrorBody(res: Response): Promise<EventError> {
  let code: ApiErrorCode | 'http_error' = 'http_error'
  let message = `HTTP ${res.status}`
  let runId: string | undefined
  const text = await res.text().catch(() => '')
  if (text) {
    try {
      const body = JSON.parse(text) as Partial<ApiErrorBody>
      const inner = body?.error
      if (inner && typeof inner === 'object') {
        if (typeof inner.code === 'string') code = inner.code as ApiErrorCode
        if (typeof inner.message === 'string' && inner.message) message = inner.message
        if (typeof inner.run_id === 'string' && inner.run_id) runId = inner.run_id
      }
    } catch {
      message = text.slice(0, 300)
    }
  }
  return new EventError(message, code, res.status, runId)
}

/**
 * 建立 SSE 连接并持续回调。同步返回 AbortController 供取消。
 *
 * 语义保证：
 * - 成功打开时恰好调用一次 onOpen
 * - 流正常结束 / 被 abort 时恰好调用一次 onClose
 * - 出错时恰好调用一次 onError（abort 不算错误，不会触发 onError）
 * - onClose 一定在 onError 之后调用（如果有错误）
 *
 * 注：这里删掉了两个「声明了但从不读取」的局部符号（createAbortError / opened），
 * 它们没有调用点，纯属死代码；tsconfig 开了 noUnusedLocals，留着会让 vue-tsc 报错。
 * 行为完全不变。
 */
export function streamSse(url: string, init: RequestInit, handlers: SseHandlers): AbortController {
  const controller = new AbortController()
  const { onEvent, onOpen, onClose, onError, onChunk } = handlers

  let settled = false
  const finish = (err?: Error) => {
    if (settled) return
    settled = true
    if (err && onError) {
      try {
        onError(err)
      } catch (e) {
        console.error('[sse] onError handler threw', e)
      }
    }
    if (onClose) {
      try {
        onClose()
      } catch (e) {
        console.error('[sse] onClose handler threw', e)
      }
    }
  }

  const run = async (): Promise<void> => {
    const signal = controller.signal

    // 用户传进来的 signal（如果有）一并联动
    const outer = init.signal
    if (outer) {
      if (outer.aborted) controller.abort()
      else outer.addEventListener('abort', () => controller.abort(), { once: true })
    }

    let res: Response
    try {
      res = await fetch(url, {
        ...init,
        method: init.method ?? 'GET',
        headers: {
          Accept: 'text/event-stream',
          'Cache-Control': 'no-cache',
          ...(init.headers as Record<string, string> | undefined),
        },
        signal,
      })
    } catch (err) {
      if (signal.aborted) return finish()
      return finish(new NetworkError(`无法连接后端（${url}）`, err))
    }

    if (!res.ok) {
      const apiErr = await readErrorBody(res)
      if (signal.aborted) return finish()
      return finish(apiErr)
    }
    if (!res.body) {
      return finish(new EventError('当前环境不支持流式响应（response.body 为空）', 'http_error', res.status))
    }

    if (onOpen) {
      try {
        onOpen()
      } catch (e) {
        console.error('[sse] onOpen handler threw', e)
      }
    }

    const reader = res.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buffer = ''

    const flushBlock = (block: string): void => {
      let parsed: RawEvent | null
      try {
        parsed = parseEventBlock(block)
      } catch (e) {
        console.warn('[sse] 事件块解析失败，已跳过：', toError(e).message)
        return
      }
      if (!parsed) return // 纯注释 / 心跳
      const payload = decodeData(parsed.data)
      try {
        onEvent(parsed.event, payload)
      } catch (e) {
        // 消费端抛错不能拖垮读取循环
        console.error('[sse] onEvent 处理异常：', e)
      }
    }

    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        if (onChunk) {
          try {
            onChunk()
          } catch (e) {
            console.error('[sse] onChunk handler threw', e)
          }
        }
        buffer += decoder.decode(value, { stream: true })

        // 按空行切块：兼容 \n\n、\r\n\r\n、\r\r
        for (;;) {
          const match = BLOCK_SEPARATOR.exec(buffer)
          if (!match || match.index === undefined) break
          const block = buffer.slice(0, match.index)
          buffer = buffer.slice(match.index + match[0].length)
          if (block.trim() !== '') flushBlock(block)
        }
      }

      // 收尾：把解码器里剩下的多字节字符和最后一个未以空行结束的事件补上
      buffer += decoder.decode()
      if (buffer.trim() !== '') {
        for (const block of buffer.split(BLOCK_SEPARATOR)) {
          if (block.trim() !== '') flushBlock(block)
        }
      }
      buffer = ''
      finish()
    } catch (err) {
      // reader.read() 失败或 decoder 失败
      if (signal.aborted) return finish()
      const e = toError(err)
      if (e.name === 'AbortError') return finish()
      finish(e)
    } finally {
      try {
        reader.releaseLock()
      } catch {
        /* 已被取消，忽略 */
      }
    }
  }

  void run()

  return controller
}

/** 匹配 SSE 事件块之间的空行分隔符（兼容 \n\n、\r\n\r\n、\r\r） */
const BLOCK_SEPARATOR = /\r\n\r\n|\n\n|\r\r/

/** 供 store 复用的静默超时默认值（后端心跳 15s，留 3 倍余量） */
export const DEFAULT_STALL_TIMEOUT_MS = 45_000
