/**
 * 极简 fetch 封装（v2 契约）。
 * - 所有非 2xx 响应统一解析成 EventError（带 code / message / status）。
 * - 后端错误体形状固定为 { "error": { "code", "message" } }。
 * - 204 / 空 body 返回 undefined。
 * - 不引 axios，不引任何库。
 *
 * 注意：命令类接口只返回 ack，**不含内容**。内容一律从 GET /api/stream 下来。
 */

import type {
  ApiErrorBody,
  ApiErrorCode,
  Attachment,
  Conversation,
  CreateConversationInput,
  CreateProviderInput,
  DefaultsInput,
  EditMessageInput,
  Health,
  Me,
  Message,
  Model,
  Provider,
  ProviderModelsFetchResponse,
  ProviderTestResponse,
  SendMessageInput,
  SendMessageResponse,
  SettingsResponse,
  State,
  UpdateConversationInput,
  UpdateProviderInput,
  UploadedFile,
  UploadResponse,
  UserInfo,
} from './types'

/* ------------------------------------------------------------------ */
/* 访问令牌                                                            */
/* ------------------------------------------------------------------ */

const TOKEN_KEY = 'ai-assistant-token'

/**
 * 访问令牌（后端 -token 起的时候才需要）。
 *
 * 常规路径根本用不到它：手机第一次带 ?token= 打开，服务端就下发了一年期的 cookie，
 * 之后 fetch / SSE / <img> 都自动带上。这里存一份是为了兜住「cookie 被禁用」的环境
 * （隐私模式、浏览器设置里关掉了 cookie）：那时靠 X-Auth-Token 头仍然能把接口打通。
 *
 * 注意：cookie 才是主路径。只靠这个头的话，`<img src>` 那种加不了 header 的请求会 401，
 * 图片显示不出来（表现为一批 /api/attachments 下的 thumb 请求失败）。
 */
let authToken = readStoredToken()

function readStoredToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

/**
 * 让 cookie 与 localStorage 里的令牌保持一致。
 *
 * 早期版本手输令牌只存了 localStorage，导致 `<img>`（加不了 header）401、图片红叹号。
 * 这里启动时自愈：有令牌但 cookie 缺失就补种。cookie 属性与服务端 bootstrapToken
 * 下发的一致（一年期、SameSite=Strict；不带 Secure 是为了兼容内网明文 http）。
 */
const TOKEN_COOKIE = 'ai_token'

function ensureTokenCookie(): void {
  if (!authToken) return
  try {
    if (document.cookie.includes(`${TOKEN_COOKIE}=`)) return
    document.cookie = `${TOKEN_COOKIE}=${encodeURIComponent(authToken)}; path=/; max-age=${365 * 24 * 3600}; samesite=strict`
  } catch {
    /* cookie 被禁用的环境：<img> 依旧 401，API 还能走 header */
  }
}
ensureTokenCookie()

export function getAuthToken(): string {
  return authToken
}

/** 设置令牌；空串表示清除。写入失败（隐私模式）不影响本次会话使用。 */
export function setAuthToken(value: string): void {
  authToken = value.trim()
  try {
    if (authToken) localStorage.setItem(TOKEN_KEY, authToken)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* 隐私模式下 localStorage 可能抛错，内存里那份仍然有效 */
  }
  // cookie 同步：img 等加不了 header 的请求靠它
  if (authToken) ensureTokenCookie()
  else {
    try {
      document.cookie = `${TOKEN_COOKIE}=; path=/; max-age=0`
    } catch {
      /* 同上 */
    }
  }
}

/** 给所有出站请求统一的鉴权头（没有令牌时返回空对象） */
export function authHeaders(): Record<string, string> {
  return authToken ? { 'X-Auth-Token': authToken } : {}
}

/** 是不是「要令牌但没给对」这种 401；UI 据此弹输入框而不是当成网络故障重试 */
export function isUnauthorized(err: unknown): boolean {
  return err instanceof EventError && err.status === 401
}

/**
 * 401 的上报钩子。
 *
 * client.ts 刻意不依赖 Vue（它要能脱离框架单测），所以这里用回调而不是响应式状态；
 * App.vue 挂载时注册，收到就弹「输入访问令牌」。
 */
type UnauthorizedHandler = () => void
let unauthorizedHandler: UnauthorizedHandler | null = null

export function setUnauthorizedHandler(fn: UnauthorizedHandler | null): void {
  unauthorizedHandler = fn
}

export function notifyUnauthorized(err: unknown): void {
  if (!isUnauthorized(err)) return
  try {
    unauthorizedHandler?.()
  } catch {
    /* 钩子里出错不影响调用方 */
  }
}

export class EventError extends Error {
  readonly code: ApiErrorCode | 'http_error'
  readonly status: number
  /** v1 遗留字段：api/sse.ts 的错误体解析会把它传进来，保留以免改动那个文件 */
  readonly runId?: string

  constructor(
    message: string,
    code: ApiErrorCode | 'http_error' = 'http_error',
    status = 0,
    runId?: string,
  ) {
    super(message)
    this.name = 'EventError'
    this.code = code
    this.status = status
    if (runId !== undefined) this.runId = runId
  }

  /** 该错误是否是契约里定义的错误体（而不是网络/解析异常） */
  get isApiError(): boolean {
    return this.status > 0
  }
}

/** 网络层失败（DNS/连接被拒/CORS）也包成 EventError，方便 UI 统一提示 */
export class NetworkError extends EventError {
  constructor(message: string, cause?: unknown) {
    super(message, 'http_error', 0)
    this.name = 'NetworkError'
    if (cause !== undefined) {
      ;(this as { cause?: unknown }).cause = cause
    }
  }
}

/** 把任意 throw 出来的东西转成 Error，避免 UI 里出现 "[object Object]" */
export function toError(err: unknown): Error {
  if (err instanceof Error) return err
  if (typeof err === 'string') return new Error(err)
  try {
    return new Error(JSON.stringify(err))
  } catch {
    return new Error('未知错误')
  }
}

/** 取出契约里的错误码；不是契约错误体（网络/解析异常）时返回 '' */
export function errorCode(err: unknown): ApiErrorCode | 'http_error' | '' {
  return err instanceof EventError && err.status > 0 ? err.code : ''
}

async function parseErrorBody(res: Response): Promise<EventError> {
  let code: ApiErrorCode | 'http_error' = 'http_error'
  let message = `HTTP ${res.status}`

  const text = await res.text().catch(() => '')
  if (text) {
    try {
      const body = JSON.parse(text) as Partial<ApiErrorBody>
      const inner = body?.error
      if (inner && typeof inner === 'object') {
        if (typeof inner.code === 'string') code = inner.code as ApiErrorCode
        if (typeof inner.message === 'string' && inner.message) message = inner.message
      }
    } catch {
      // 不是 JSON：直接用截断后的原始文本，别把整个 HTML 错误页塞进 toast
      message = text.slice(0, 300)
    }
  }
  const err = new EventError(message, code, res.status)
  // 401 说明后端开了 -token 而我们没有（或给错了）：让 UI 弹输入框
  notifyUnauthorized(err)
  return err
}

export type QueryValue = string | number | boolean | null | undefined

export interface RequestOptions {
  query?: Record<string, QueryValue>
  signal?: AbortSignal
  headers?: Record<string, string>
}

function buildUrl(path: string, query?: Record<string, QueryValue>): string {
  if (!query) return path
  const usp = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null) continue
    usp.append(k, String(v))
  }
  const qs = usp.toString()
  return qs ? `${path}?${qs}` : path
}

/** 统一入口：任何 HTTP 错误都抛 EventError */
export async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  options: RequestOptions = {},
): Promise<T> {
  const url = buildUrl(path, options.query)
  const headers: Record<string, string> = {
    Accept: 'application/json',
    ...authHeaders(),
    ...(options.headers ?? {}),
  }
  const init: RequestInit = { method, headers }
  if (options.signal) init.signal = options.signal

  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }

  let res: Response
  try {
    res = await fetch(url, init)
  } catch (err) {
    // fetch 只在网络层失败时 reject；如果调用方主动 abort，原样抛出
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new NetworkError(`无法连接后端（${method} ${path}）`, err)
  }

  if (!res.ok) throw await parseErrorBody(res)

  if (res.status === 204 || res.status === 205) return undefined as T

  const text = await res.text()
  if (!text) return undefined as T
  try {
    return JSON.parse(text) as T
  } catch {
    throw new EventError(`响应不是合法 JSON（${method} ${path}）`, 'http_error', res.status)
  }
}

export const http = {
  get: <T>(path: string, options?: RequestOptions): Promise<T> => request<T>('GET', path, undefined, options),
  post: <T>(path: string, body?: unknown, options?: RequestOptions): Promise<T> =>
    request<T>('POST', path, body, options),
  patch: <T>(path: string, body?: unknown, options?: RequestOptions): Promise<T> =>
    request<T>('PATCH', path, body, options),
  put: <T>(path: string, body?: unknown, options?: RequestOptions): Promise<T> =>
    request<T>('PUT', path, body, options),
  delete: <T = void>(path: string, options?: RequestOptions): Promise<T> =>
    request<T>('DELETE', path, undefined, options),
}

/* ------------------------------------------------------------------ */
/* 上传（multipart，不能用通用 request()：它会把 body JSON.stringify）    */
/* ------------------------------------------------------------------ */

/** 契约 §4：multipart 字段名固定是 file，可以重复追加多个 */
const UPLOAD_FIELD = 'file'
const UPLOAD_PATH = '/api/uploads'

/**
 * 上传图片，返回后端登记的附件列表（契约 §4）。
 *
 * - 字段名必须是 `file`，多个文件重复 append 同一个字段名；
 * - **绝不手动设置 Content-Type**：必须让浏览器自己带上
 *   `multipart/form-data; boundary=…`，手写会漏掉 boundary，后端解不出来；
 * - 顺序与入参一致（后端响应里的 `attachments` 按上传顺序返回）；
 * - 失败时抛 EventError，message 就是后端 `error.message`
 *   （例如「图片超过了 20 MB 的上限」），调用方直接展示即可。
 */
export async function uploadAttachments(
  files: File[],
  options: RequestOptions = {},
): Promise<Attachment[]> {
  if (!Array.isArray(files) || files.length === 0) return []

  const form = new FormData()
  for (const file of files) form.append(UPLOAD_FIELD, file)

  const init: RequestInit = {
    method: 'POST',
    // 注意：这里**只**有 Accept 和鉴权头。没有 Content-Type —— 那是故意的，见上面的注释。
    headers: { Accept: 'application/json', ...authHeaders(), ...(options.headers ?? {}) },
    body: form,
  }
  if (options.signal) init.signal = options.signal

  let res: Response
  try {
    res = await fetch(UPLOAD_PATH, init)
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new NetworkError('无法连接后端（POST /api/uploads）', err)
  }

  if (!res.ok) throw await parseErrorBody(res)

  const text = await res.text()
  if (!text) return []
  let body: Partial<UploadResponse>
  try {
    body = JSON.parse(text) as Partial<UploadResponse>
  } catch {
    throw new EventError('上传响应不是合法 JSON', 'http_error', res.status)
  }
  return Array.isArray(body?.attachments) ? body.attachments : []
}

/** 上传文档（txt/md/csv/json/代码/PDF/DOCX/XLSX），返回文件记录（id 用于发消息和 read_file） */
export async function uploadFiles(
  files: File[],
  options: RequestOptions = {},
): Promise<UploadedFile[]> {
  if (!Array.isArray(files) || files.length === 0) return []
  const form = new FormData()
  for (const file of files) form.append(UPLOAD_FIELD, file)
  const init: RequestInit = {
    method: 'POST',
    headers: { Accept: 'application/json', ...authHeaders(), ...(options.headers ?? {}) },
    body: form,
  }
  if (options.signal) init.signal = options.signal
  let res: Response
  try {
    res = await fetch('/api/files', init)
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new NetworkError('无法连接后端（POST /api/files）', err)
  }
  if (!res.ok) throw await parseErrorBody(res)
  const body = JSON.parse(await res.text()) as { files?: UploadedFile[] }
  return Array.isArray(body?.files) ? body.files : []
}

/* ------------------------------------------------------------------ */
/* 契约里的具体端点（集中在这里，组件/store 不直接写 URL）                 */
/* ------------------------------------------------------------------ */

export const api = {
  health: (options?: RequestOptions) => http.get<Health>('/api/health', options),

  /* 当前用户 / 用户管理（管理员） */
  getMe: (options?: RequestOptions) => http.get<Me>('/api/me', options),
  listUsers: (options?: RequestOptions) => http.get<UserInfo[]>('/api/users', options),
  createUser: (name: string, options?: RequestOptions) =>
    http.post<{ user: UserInfo; token: string }>('/api/users', { name }, options),
  rotateUserToken: (id: number, options?: RequestOptions) =>
    http.post<{ token: string }>(`/api/users/${id}/token`, undefined, options),
  deleteUser: (id: number, options?: RequestOptions) => http.delete<void>(`/api/users/${id}`, options),

  /* 那条唯一的 SSE 长连接 */
  streamUrl: () => '/api/stream',

  /* 设置 / 供应商 / 模型 */
  getSettings: (options?: RequestOptions) => http.get<SettingsResponse>('/api/settings', options),
  /** 新建会话的默认值（供应商 / 模型 / 思考强度）；字段省略 = 不改，空串或 0 = 清空 */
  updateDefaults: (input: DefaultsInput, options?: RequestOptions) =>
    http.put<SettingsResponse>('/api/settings/defaults', input, options),

  /* ---- 供应商 CRUD（可以配任意多家） ---- */
  listProviders: (options?: RequestOptions) => http.get<Provider[]>('/api/providers', options),
  createProvider: (input: CreateProviderInput, options?: RequestOptions) =>
    http.post<Provider>('/api/providers', input, options),
  /** api_key 省略或空串 = 保留原 key 不变 */
  patchProvider: (id: number, input: UpdateProviderInput, options?: RequestOptions) =>
    http.patch<Provider>(`/api/providers/${id}`, input, options),
  /** 级联删掉它的模型缓存；引用它的会话会回退到默认供应商 */
  deleteProvider: (id: number, options?: RequestOptions) =>
    http.delete<void>(`/api/providers/${id}`, options),

  /* ---- 单个供应商的操作 ---- */
  fetchProviderModels: (id: number, options?: RequestOptions) =>
    http.post<ProviderModelsFetchResponse>(`/api/providers/${id}/models/fetch`, undefined, options),
  /** 注意：上游报错时它也返回 HTTP 200 且 ok:false，message 才是错误说明 */
  testProvider: (id: number, modelId?: string, options?: RequestOptions) =>
    http.post<ProviderTestResponse>(
      `/api/providers/${id}/test`,
      modelId ? { model_id: modelId } : {},
      options,
    ),

  /* ---- 模型 ---- */
  /** 省略 providerId = 所有供应商的模型；每项都带 provider_id */
  listModels: (providerId?: number, options?: RequestOptions) =>
    http.get<Model[]>(
      '/api/models',
      providerId === undefined ? options : { ...options, query: { ...options?.query, provider_id: providerId } },
    ),

  /* 会话 */
  listConversations: (options?: RequestOptions) => http.get<Conversation[]>('/api/conversations', options),
  createConversation: (input: CreateConversationInput = {}, options?: RequestOptions) =>
    http.post<Conversation>('/api/conversations', input, options),
  patchConversation: (id: number, input: UpdateConversationInput, options?: RequestOptions) =>
    http.patch<{ conv: Conversation }>(`/api/conversations/${id}`, input, options),
  forkConversation: (id: number, beforeMessageID?: number, options?: RequestOptions) =>
    http.post<{ conv: Conversation }>(
      `/api/conversations/${id}/fork`,
      beforeMessageID ? { before_message_id: beforeMessageID } : undefined,
      options,
    ),

  /** 手动压缩上下文：ack 即返回，进度走 SSE（conv.compacting + notice） */
  compactConversation: (id: number, options?: RequestOptions) =>
    http.post<{ accepted: boolean }>(`/api/conversations/${id}/compact`, undefined, options),
  /** 向上翻页：before 之前（不含）的更早消息，正序 */
  listMessages: (convId: number, before: number, limit = 20, options?: RequestOptions) =>
    http.get<Message[]>(`/api/conversations/${convId}/messages?before=${before}&limit=${limit}`, options),
  deleteConversation: (id: number, options?: RequestOptions) =>
    http.delete<void>(`/api/conversations/${id}`, options),
  /** 权威快照：conv + seq + generating + messages */
  convState: (id: number, options?: RequestOptions) =>
    http.get<State>(`/api/conversations/${id}/state`, options),
  /** 标记会话已读（侧边栏小红点据此熄灭） */
  markConversationRead: (id: number, options?: RequestOptions) =>
    http.post<{ conv: Conversation }>(`/api/conversations/${id}/read`, undefined, options),

  /* 附件上传（契约 §4：multipart/form-data，字段名 file，可重复） */
  uploadAttachments,

  /* 命令（只返回 ack） */
  sendMessage: (convId: number, input: SendMessageInput, options?: RequestOptions) =>
    http.post<SendMessageResponse>(`/api/conversations/${convId}/messages`, input, options),
  stop: (convId: number, options?: RequestOptions) =>
    http.post<{ stopped: boolean }>(`/api/conversations/${convId}/stop`, undefined, options),
  regenerate: (messageId: number, options?: RequestOptions) =>
    http.post<{ accepted: boolean }>(`/api/messages/${messageId}/regenerate`, undefined, options),
  patchMessage: (id: number, input: EditMessageInput, options?: RequestOptions) =>
    http.patch<{ message: Message }>(`/api/messages/${id}`, input, options),
  restoreMessage: (id: number, options?: RequestOptions) =>
    http.post<{ message: Message }>(`/api/messages/${id}/restore`, undefined, options),
  restoreMessageReasoning: (id: number, options?: RequestOptions) =>
    http.post<{ message: Message }>(`/api/messages/${id}/restore-reasoning`, undefined, options),
  deleteMessage: (id: number, options?: RequestOptions) =>
    http.delete<void>(`/api/messages/${id}`, options),
}
