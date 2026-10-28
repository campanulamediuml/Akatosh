/**
 * 与后端契约 (API.md v2) 一一对应的 TypeScript 类型。
 * 只做「照抄」，不在这里发明契约里没有的字段。
 *
 * v2 心智模型（决定了下面类型的形状）：
 * - 只有一条 SSE 长连接 `GET /api/stream`，所有会话的事件都从它下来；
 * - 上行命令是普通 POST，只返回 ack，内容一律走事件；
 * - 前端不持有真相，`State` 是唯一的权威快照，`seq` 是它的高水位。
 */

/* ------------------------------------------------------------------ */
/* 错误                                                                */
/* ------------------------------------------------------------------ */

/** 契约 §0 定义的错误码 */
export type ApiErrorCode =
  | 'bad_request'
  | 'not_found'
  | 'no_provider'
  | 'no_model'
  | 'busy'
  | 'compacting'
  | 'archived'
  | 'upstream_error'
  | 'internal'

/** 后端错误体：{ "error": { "code", "message" } } */
export interface ApiErrorBody {
  error: {
    code: ApiErrorCode
    message: string
    /** v1 遗留字段，后端已不再返回；保留是为了让 sse.ts 的解析逻辑不必改动 */
    run_id?: string
  }
}

/* ------------------------------------------------------------------ */
/* 0. 健康检查                                                          */
/* ------------------------------------------------------------------ */

export interface Health {
  ok: boolean
  version: string
  /** 当前挂在 SSE 总线上的前端连接数（诊断用） */
  stream_clients?: number
}

/* ------------------------------------------------------------------ */
/* 1. 供应商 / 模型 / 设置                                               */
/* ------------------------------------------------------------------ */

export interface Provider {
  id: number
  name: string
  base_url: string
  /** 后端做过归一化的地址，例如 https://api.deepseek.com/v1 */
  normalized_base_url: string
  api_key_set: boolean
  /** 例如 "sk-12****abcd"，永不含明文 */
  api_key_hint: string
  /** 这家供应商名下缓存的模型数（GET /api/providers 直接带出来） */
  model_count: number
  /** Unix 毫秒 */
  created_at: number
}

export interface SettingsDefaults {
  /**
   * 新建会话用哪家供应商；0 = 没设过（后端会自己挑一个）。
   * 与 Conversation.provider_id 同一个含义。
   */
  provider_id: number
  model_id: string
  effort: string
  /** 全局默认系统提示词：新建会话时带上（之后改它不影响老会话） */
  system_prompt?: string
}

/**
 * GET /api/settings 与 PUT /api/settings/defaults 的响应（同形状）。
 * 注意 v2 里**没有**单个 `provider` 字段了：供应商是一张列表。
 */
/** 消息搜索结果（GET /api/search/messages） */
export interface SearchResult {
  conv_id: number
  title: string
  message_id: number
  role: string
  snippet: string
  created_at: number
  match_count: number
}

/* ------------------------------------------------------------------ */
/* 用户（多租户）                                                        */
/* ------------------------------------------------------------------ */

/** 当前登录用户（GET /api/me） */
export interface Me {
  id: number
  name: string
  is_admin: boolean
}

/** 用户管理列表项（GET /api/users，管理员） */
export interface UserInfo {
  id: number
  name: string
  is_admin: boolean
  created_at: number
  last_seen_at: number
}

export interface SettingsResponse {
  /** 与 GET /api/providers 相同，省一次请求；按 id 升序 */
  providers: Provider[]
  defaults: SettingsDefaults
  /** 后端是否配置了搜索后端（-searxng）；false 时前端隐藏「联网」开关 */
  web_search_available?: boolean
  /** 上下文压缩阈值（0~1，0 = 禁用）；进度条详情里展示「达到 X% 触发自动压缩」 */
  compact_threshold?: number
}

/** POST /api/providers 请求体 */
export interface CreateProviderInput {
  name?: string
  base_url: string
  api_key: string
}

/** PATCH /api/providers/{id} 请求体；api_key 省略或空串表示保留原 key */
export interface UpdateProviderInput {
  name?: string
  base_url?: string
  api_key?: string
}

/**
 * PUT /api/settings/defaults 请求体。
 * 字段省略 = 不改动；空串 / 0 = 清空该项。
 */
export interface DefaultsInput {
  provider_id?: number
  model_id?: string
  effort?: string
  system_prompt?: string
}

export interface Model {
  id: number
  /** 这条模型记录属于哪家供应商（同名模型在不同供应商下是两条记录） */
  provider_id: number
  model_id: string
  display_name: string
  /** 是否支持思考 */
  reasoning: boolean
  /** 思考强度下拉的唯一数据来源；空数组 = 不支持 */
  efforts: string[]
  context_len: number
  /** Unix 毫秒 */
  fetched_at: number
}

/**
 * 「供应商 + 模型名」的复合键。
 * 模型名本身可能含 `/`（例如 OpenRouter 的 `openai/gpt-4o`），所以用数字
 * provider_id 打头、冒号分隔：前缀一定是纯数字，不会和模型名里的字符混淆。
 */
export function modelKey(providerId: number, modelId: string): string {
  return `${providerId}:${modelId}`
}

export interface ProviderModelsFetchResponse {
  total: number
  added: number
  models: Model[]
}

export interface ProviderTestResponse {
  ok: boolean
  latency_ms: number
  message: string
  model_id?: string
}

/**
 * 思考强度取值 → 中文标签。
 * 注意 efforts 是后端从上游拿到的字符串数组，不一定只有这几个值，
 * 取不到标签时一律原样显示。
 */
export const EFFORT_LABELS: Readonly<Record<string, string>> = {
  minimal: '极简',
  low: '低',
  medium: '中',
  high: '高',
  max: '最高',
}

/* ------------------------------------------------------------------ */
/* 2. 会话                                                             */
/* ------------------------------------------------------------------ */

export interface Conversation {
  id: number
  title: string
  /**
   * 会话绑定到具体供应商：provider_id + model_id 一起决定这条对话发给谁。
   * 两个网关都有 gpt-4o 时靠 provider_id 区分。
   */
  provider_id: number
  /** 只读冗余字段，后端按 provider_id 关联出来的名字，方便界面直接显示 */
  provider_name: string
  model_id: string
  effort: string
  system_prompt: string
  /**
   * 上下文压缩：Summary 是旧消息的压缩摘要，CompactedBefore 是摘要覆盖到的
   * 消息 id（含）。更早的消息已被冻结（只读）。
   */
  summary: string
  compacted_before: number
  /** 压缩后对下一次请求 prompt tokens 的估算；0 = 使用上游实测值 */
  context_estimate: number
  /** 该会话此刻是否正在压缩（内存态；压缩期间阻塞发送） */
  compacting: boolean
  pinned: boolean
  archived: boolean
  message_count: number
  created_at: number
  updated_at: number
  /**
   * 该会话是否正在生成回复。
   * 后端保证任何来源的 Conversation 都带这个字段且是权威值：
   * 会话列表、state 快照、以及每次生成开始/结束各广播一次的 conv 事件。
   * 侧栏的脉冲小圆点直接用它。
   */
  generating: boolean
  /**
   * 有「比你上次查看更新的、已经答完的」助手消息。
   * 只统计已经答完的（finished_at > 0），所以正在生成时不会亮，
   * 答完那一刻才亮 —— 这就是「提示可以看了」。
   * 用户切到这个会话、或看着它时回答落地，后端会刷新 last_read_at 把它清掉。
   */
  unread: boolean
  /** 最近一次被查看的时间戳（Unix 毫秒） */
  last_read_at: number
  /** 联网搜索开关：开启后生成时模型可自主调用 web_search 工具 */
  web_search: boolean
}

/** /api/files 上传返回的一条文件记录 */
export interface UploadedFile {
  id: number
  name: string
  ext: string
  size: number
}

export interface CreateConversationInput {
  title?: string
  /** 省略时用 PUT /api/settings/defaults 里保存的默认供应商 */
  provider_id?: number
  model_id?: string
  effort?: string
}

/**
 * PATCH /api/conversations/{id} 请求体。
 *
 * 契约提醒：**只改 provider_id 不改 model_id** 时后端会把 model_id 清空，
 * 由前端随后把 model_id 设成新供应商下的某个模型。本前端不做这种两步操作：
 * 换模型时一律同时带上 provider_id + model_id（见 ModelPicker.vue）。
 */
export interface UpdateConversationInput {
  title?: string
  provider_id?: number
  model_id?: string
  effort?: string
  pinned?: boolean
  archived?: boolean
  system_prompt?: string
  web_search?: boolean
}

/** State.generating：该会话正在生成时的实时信息 */
export interface Generating {
  message_id: number
  provider_id: number
  model_id: string
  effort: string
  started_at: number
  reasoning_len: number
  content_len: number
}

/** GET /api/conversations/{id}/state 的响应：一份完整的权威快照 */
export interface State {
  conv: Conversation | null
  /** 这份快照已经包含到哪个序号；前端把它当作 lastSeq 的高水位 */
  seq: number
  generating: Generating | null
  /** 最近若干条（首屏分页）；更早的用 listMessages 向上翻 */
  messages: Message[]
  /** 是否还有更早的消息可翻 */
  has_more: boolean
}

/* ------------------------------------------------------------------ */
/* 3. 消息                                                             */
/* ------------------------------------------------------------------ */

export type MessageRole = 'user' | 'assistant' | 'system'
export type MessageStatus = 'streaming' | 'done' | 'error' | 'aborted'

export interface Usage {
  prompt_tokens: number
  completion_tokens: number
  reasoning_tokens: number
  total_tokens: number
  /** 前缀缓存命中/未命中；上游不报就是 0 */
  cache_hit_tokens: number
  cache_miss_tokens: number
}

export const EMPTY_USAGE: Readonly<Usage> = {
  prompt_tokens: 0,
  completion_tokens: 0,
  reasoning_tokens: 0,
  total_tokens: 0,
  cache_hit_tokens: 0,
  cache_miss_tokens: 0,
}

export interface Message {
  id: number
  conv_id: number
  role: MessageRole
  status: MessageStatus
  content: string
  /** 完整 CoT，可能为空串 */
  reasoning: string
  /** content 是否被用户改过 */
  edited: boolean
  /** 仅当 edited=true 时非空，用于「还原原文」 */
  content_original: string
  /** 非空 = CoT 被编辑过（存原文，可还原）；编辑后会随历史回传给上游 */
  reasoning_original: string
  /** 这条消息实际发给了哪家供应商（多供应商下同名模型靠它分辨） */
  provider_id: number
  model_id: string
  effort: string
  error: string
  /** 本轮联网搜索的记录（JSON 数组：[{query, results:[{title,url}], error?}]），空串 = 没搜 */
  search_log: string
  usage: Usage
  /** 工具循环各段流各自的 usage（有工具调用的消息才有）；usage 字段是累计成本口径 */
  seg_usages?: Usage[]
  created_at: number
  finished_at: number
  /**
   * 这条消息带的附件（图片）。
   * 直接来自后端（`state.messages[].attachments` 与 SSE `message` 事件都带），
   * 前端**不要自己拼**这个数组。
   */
  attachments?: Attachment[]
  /** 这条消息挂的文档（/api/files 上传的）；read_file 工具按 id 读取 */
  files?: UploadedFile[]
}

/* ------------------------------------------------------------------ */
/* 3.1 附件（图片）— 契约 §4                                            */
/* ------------------------------------------------------------------ */

/** 目前契约只定义了 image；留着联合类型是为了以后加视频/文件时不至于改类型名 */
export type AttachmentKind = 'image'

/**
 * 上传后的附件记录。
 *
 * 性能硬约束（契约 §4「前端硬约束」）：
 * - 消息列表里**只**用 `thumb_url`（JPEG，长边 320，通常 10~30 KB）；
 * - `raw_url` 只在用户点开灯箱时请求，任何地方都不许预加载。
 */
export interface Attachment {
  id: number
  kind: AttachmentKind
  /** 例如 "image/png" */
  mime: string
  /** 原始文件名，例如 "shot.png" */
  name: string
  /** 原始文件字节数 */
  size: number
  /** 原图宽（像素） */
  width: number
  /** 原图高（像素） */
  height: number
  /** 缩略图地址：消息列表里唯一允许请求的图片地址 */
  thumb_url: string
  /** 原图地址：只在 ImageViewer 里被引用 */
  raw_url: string
  /** Unix 毫秒 */
  created_at: number
}

/** POST /api/uploads 的响应 */
export interface UploadResponse {
  attachments: Attachment[]
}

/* ------------------------------------------------------------------ */
/* 4. 上行命令（都只返回 ack，内容走 SSE）                                */
/* ------------------------------------------------------------------ */

export interface SendMessageInput {
  /** 契约 §4：允许为空串（只发图） */
  content: string
  /** 上传拿到的附件 id；与 content 至少有一个非空 */
  attachment_ids?: number[]
  /** /api/files 上传拿到的文档 id（read_file 工具可读） */
  file_ids?: number[]
  /** 省略时用会话自身的设置 */
  model_id?: string
  effort?: string
  client_msg_id?: string
}

export interface SendMessageResponse {
  accepted: boolean
  user_message_id: number
  assistant_message_id: number
}

export interface EditMessageInput {
  content?: string
  /** CoT 编辑（编辑后会随历史回传给上游） */
  reasoning?: string
}

/* ------------------------------------------------------------------ */
/* 5. SSE 事件                                                         */
/* ------------------------------------------------------------------ */

export const SSE_EVENTS = {
  hello: 'hello',
  delta: 'delta',
  message: 'message',
  conv: 'conv',
  notice: 'notice',
} as const

export type SseEventName = (typeof SSE_EVENTS)[keyof typeof SSE_EVENTS]

/** 每个事件都带 seq：全局单调递增的发布序号 */
export interface SeqPayload {
  seq: number
}

export interface HelloPayload extends SeqPayload {
  client_id: string
  server_time: number
}

/** delta 是唯一的增量事件，前端只做追加 */
export type DeltaChannel = 'reasoning' | 'content'

export interface DeltaPayload extends SeqPayload {
  conv_id: number
  message_id: number
  /** reasoning（CoT）或 content（正文） */
  channel: DeltaChannel | string
  text: string
}

export type MessageAction = 'created' | 'updated' | 'deleted' | 'truncated'

export interface MessagePayload extends SeqPayload {
  conv_id: number
  action: MessageAction | string
  message_id: number
  /** created / updated 一定带完整 message；deleted / truncated 不带 */
  message?: Message | null
}

export type ConvAction = 'updated' | 'deleted'

export interface ConvPayload extends SeqPayload {
  conv_id: number
  action: ConvAction | string
  conv?: Conversation | null
}

export type NoticeLevel = 'info' | 'warn' | 'error'

export interface NoticePayload extends SeqPayload {
  conv_id?: number
  level: NoticeLevel | string
  text: string
}

/** 把 SSE data 里的 seq 安全地取出来（缺字段/类型不对时返回 0 = 不做高水位判断） */
export function payloadSeq(data: unknown): number {
  if (!data || typeof data !== 'object') return 0
  const seq = (data as { seq?: unknown }).seq
  return typeof seq === 'number' && Number.isFinite(seq) ? seq : 0
}

/** 把 SSE data 里的 conv_id 安全地取出来 */
export function payloadConvId(data: unknown): number | null {
  if (!data || typeof data !== 'object') return null
  const id = (data as { conv_id?: unknown }).conv_id
  return typeof id === 'number' && Number.isFinite(id) ? id : null
}
