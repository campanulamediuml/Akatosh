<script setup lang="ts">
/**
 * 输入区。
 *
 * 文字部分的关键细节：
 * - textarea 自适应高度，超过 ~200px 后自身滚动
 * - Enter 发送；Ctrl+Enter / Shift+Enter（以及 Cmd/Alt+Enter）换行
 *   —— 触屏设备（hover:none，无实体键盘）反过来：Enter 只换行，发送走按钮，
 *      软键盘上没有修饰键，裸 Enter 发送会让手机用户永远没法换行
 * - 中文输入法守卫：e.isComposing || keyCode === 229 时完全放行（否则拼音选词
 *   时按回车会误发消息）
 * - 发送成功才清空输入框；409 busy 不清空（后端没落库，文字还给用户）
 * - 发送请求在飞时不禁用输入框（可以继续打字，只是发不出去）
 * - 生成中：发送按钮变成「停止」
 * - 没配置供应商 / 没有模型：输入框禁用 + 提示 + 一键打开设置
 *
 * 图片上传（契约 §4）：三种入口，同一套实现
 * - **粘贴**：`paste` 事件里带图片文件（截图后 Ctrl+V 是最高频用法）→ 上传；
 * - **拖拽**：整个输入框可 drop，拖动中边框高亮 + 提示层；drop/dragover 一律
 *   preventDefault，否则浏览器会把图片当成「打开这个文件」直接导航走；
 * - **选文件**：右下角图片按钮 → `<input type="file" accept="image/*" multiple>`。
 *   注意 input 不能用 hidden / display:none 隐藏（Android 下选择器不弹），
 *   用的是 `.file-input` 视觉透明方案，见 main.css。
 * 三条路径最终都进同一个 `addFiles()`，也就是同一个 `uploadAttachments()`，
 * 行为（过滤非图片、逐张上传、失败可重试）完全一致。
 *
 * 性能：待发送的缩略图是本地 `URL.createObjectURL(file)`，一个字节都不从服务端拉，
 * 更不会去碰原图（`raw_url` 全项目只出现在 ImageViewer 里）。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { toError, uploadAttachments, uploadFiles } from '@/api/client'
import type { Attachment } from '@/api/types'
import { useChatStore } from '@/stores/chat'
import { useSettingsStore } from '@/stores/settings'
import { noteMessageSent } from '@/stores/stream'
import { toast } from '@/stores/toast'
import { formatBytes, abbreviateNumber, formatNumber } from '@/utils/format'
import { convertHeicToJpeg, isHeic } from '@/utils/heic'
import ModelPicker from './ModelPicker.vue'

const emit = defineEmits<{ (e: 'open-settings'): void }>()

const chat = useChatStore()
const settings = useSettingsStore()

/** 超过它就让 textarea 自己滚动 */
const MAX_HEIGHT = 200

/** 与后端 media.MaxUploadBytes 一致 */
const MAX_UPLOAD_MB = 20
const MAX_UPLOAD_BYTES = MAX_UPLOAD_MB * 1024 * 1024
/** 与后端 maxUploadFileBytes 一致 */
const MAX_FILE_UPLOAD_MB = 100
const MAX_FILE_UPLOAD_BYTES = MAX_FILE_UPLOAD_MB * 1024 * 1024

const text = ref('')
const area = ref<HTMLTextAreaElement | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
/** 一次发送请求在飞：只用来防止连按回车重复投递，输入框不禁用 */
const sending = ref(false)

/**
 * 草稿按会话隔离：输入框内容跟随会话走。
 * 切会话时把当前内容存进 localStorage（按会话 id 分键），恢复目标会话的草稿；
 * 发送成功后清掉该会话的草稿。不会再出现「A 会话没发的字跟到 B 会话」。
 */
const draftKey = (id: number): string => `ai-assistant-draft-${id}`

watch(
  () => chat.conv?.id,
  (newId, oldId) => {
    if (oldId && oldId !== newId) {
      try {
        if (text.value) localStorage.setItem(draftKey(oldId), text.value)
        else localStorage.removeItem(draftKey(oldId))
      } catch { /* 隐私模式丢草稿可接受 */ }
    }
    let restored = ''
    if (newId) {
      try {
        restored = localStorage.getItem(draftKey(newId)) ?? ''
      } catch { /* 同上 */ }
    }
    text.value = restored
    void nextTick(autoGrow)
  },
)

/** 发送成功后清掉当前会话的草稿 */
function clearDraft(): void {
  const id = chat.conv?.id
  if (!id) return
  try {
    localStorage.removeItem(draftKey(id))
  } catch { /* 忽略 */ }
}

const needSettings = computed(() => !settings.hasProvider || settings.models.length === 0)

const disabledReason = computed(() => {
  if (needSettings.value) return '请先在设置里添加供应商、填好 API Key 并获取模型列表'
  if (!chat.conv) return '还没有打开会话，先新建一个对话'
  if (chat.conv.compacting) return '正在压缩上下文，请稍候…'
  return ''
})
const disabled = computed(() => disabledReason.value !== '')

/* ------------------------------------------------------------------ */
/* 待发送的图片                                                        */
/* ------------------------------------------------------------------ */

interface PendingImage {
  /** 本地唯一 key：同一张图重复添加也是两条独立记录 */
  key: string
  /** 用户选进来的原始文件 */
  file: File
  /**
   * 真正要上传的文件。HEIC/HEIF 会被前端转成 JPEG 后放这里；
   * 其它格式、以及浏览器解不了 HEIC 的情况，它就是 `file` 本身。
   */
  prepared: File | null
  /** 这个浏览器解不了 HEIC（用来给准确的提示，而不是让后端说「不是图片」） */
  heicUndecodable: boolean
  /** 本地预览地址（objectURL），用完必须 revoke */
  previewUrl: string
  status: 'uploading' | 'ready' | 'error'
  /** 上传成功后后端给的附件记录；发送时只把它的 id 交给后端 */
  attachment: Attachment | null
  /** 后端 error.message 原文，例如「图片超过了 20 MB 的上限」 */
  error: string
  /** 在飞的上传，submit 要等它 */
  promise: Promise<void> | null
  /** 移除时中断上传，避免已被删掉的图还在跑 */
  controller: AbortController | null
}

let pendingSeq = 0
const pending = ref<PendingImage[]>([])

const uploadingCount = computed(() => pending.value.filter((p) => p.status === 'uploading').length)
const readyCount = computed(() => pending.value.filter((p) => p.status === 'ready').length)
const errorCount = computed(() => pending.value.filter((p) => p.status === 'error').length)

/** 只发图不打字也算能发（契约 §4：允许 content 为空） */
const canSubmit = computed(
  () => !disabled.value && (text.value.trim().length > 0 || pending.value.length > 0 || pendingFiles.value.length > 0),
)

/* ---------------- 待发送的文档 ---------------- */

interface PendingFile {
  /** 本地唯一 key：同一份文件重复添加也是两条独立记录（与图片一致） */
  key: string
  /** 后端文件 id；上传中为 0 */
  id: number
  name: string
  size: number
  /** 原始 File 引用：失败重试用 */
  file: File
  status: 'uploading' | 'ready' | 'error'
  /** 上传失败的原因（title 悬停可见） */
  error: string
  /** 在飞的上传，submit 要等它 */
  promise: Promise<void> | null
}

let fileSeq = 0
const pendingFiles = ref<PendingFile[]>([])
const docInput = ref<HTMLInputElement | null>(null)

/**
 * 文档 input 的 accept 按平台区分：
 * - Android（Edge/Chrome 浏览器、PWA、WebView）：必须显式给出非媒体类型。
 *   裸 input（不限类型）会被系统的照片选择器接管，用户只看到「照片/拍照」；
 *   给了文档类型后 Chromium 才改用文件管理器（DocumentsUI）。
 * - iOS / 桌面：保持裸 input。iOS 的 Files 对 accept 里不认识的扩展名
 *   （.md/.py/.log 这类）会把文件灰掉，全放开才不会误伤。
 * 类型最终都由后端白名单（internal/httpapi/files.go）把关，两边保持同一份清单。
 */
const docAccept = /Android/i.test(navigator.userAgent)
  ? '.txt,.md,.markdown,.log,.csv,.json,.jsonl,.xml,.yaml,.yml,.toml,.ini,.sql,.html,.css,' +
    '.py,.js,.ts,.go,.java,.c,.h,.cpp,.rs,.sh,.pdf,.docx,.xlsx,text/plain,application/pdf'
  : ''

function pickDocs(): void {
  if (disabled.value) {
    toast.info(disabledReason.value)
    return
  }
  docInput.value?.click()
}

/**
 * 逐份上传（每份一个请求、各自有状态和重试入口——和图片的待遇一致）。
 * 注意必须 push 后再从数组里取回响应式代理去上传（同 addFiles 的坑），
 * 直接改原始对象会绕过 Vue Proxy，界面永远停在「上传中」。
 */
function onDocPicked(e: Event): void {
  const input = e.target as HTMLInputElement
  let files = Array.from(input.files ?? [])
  input.value = ''
  const oversized = files.filter((f) => f.size > MAX_FILE_UPLOAD_BYTES)
  if (oversized.length > 0) {
    toast.info(`已忽略超过 ${MAX_FILE_UPLOAD_MB} MB 的文件：${oversized.map((f) => f.name).join('、')}`)
    files = files.filter((f) => f.size <= MAX_FILE_UPLOAD_BYTES)
  }
  for (const file of files) {
    pendingFiles.value.push({
      key: `f${++fileSeq}`,
      id: 0,
      name: file.name,
      size: file.size,
      file,
      status: 'uploading',
      error: '',
      promise: null,
    })
    void uploadPending(pendingFiles.value[pendingFiles.value.length - 1])
  }
}

async function uploadPending(item: PendingFile): Promise<void> {
  item.status = 'uploading'
  item.error = ''
  const run = (async () => {
    try {
      const ups = await uploadFiles([item.file])
      const rec = ups[0]
      if (!rec || typeof rec.id !== 'number') throw new Error('服务端没有返回文件记录')
      item.id = rec.id
      item.status = 'ready'
    } catch (err) {
      item.id = 0
      item.status = 'error'
      item.error = toError(err).message
    } finally {
      item.promise = null
    }
  })()
  item.promise = run
  await run
}

function retryPendingFile(item: PendingFile): void {
  if (item.status === 'uploading') return
  void uploadPending(item)
}

function removePendingFile(key: string): void {
  pendingFiles.value = pendingFiles.value.filter((f) => f.key !== key)
}

function clearPendingFiles(): void {
  pendingFiles.value = []
}

/**
 * 是不是图片。
 * Windows 上从资源管理器拖进来的文件 MIME 可能是空串（注册表里没有该扩展名的类型），
 * 所以空 MIME 时按扩展名兜底；真不是图片的最终由后端拒掉（返回 error.message）。
 */
const IMAGE_NAME = /\.(png|jpe?g|gif|webp|bmp|avif|heic|heif)$/i

function isImageFile(file: File): boolean {
  if (file.type.startsWith('image/')) return true
  return file.type === '' && IMAGE_NAME.test(file.name)
}

/**
 * 浏览器解不了 HEIC 时给的话术。
 *
 * 比后端那句「这个文件不是能识别的图片（支持 png / jpg / gif / webp）」有用得多：
 * 文件明明是张照片，用户需要知道的是「往哪儿改」。
 */
const HEIC_HINT =
  '这个浏览器解不了 HEIC（iPhone 默认拍照格式）。' +
  '把 iPhone「设置 → 相机 → 格式」改成「兼容性最佳」再拍，或先把照片转成 JPEG。'

/**
 * 上传前的格式处理：HEIC / HEIF 用浏览器自带解码器转成 JPEG。
 *
 * 放在上传前而不是上传后重试，是为了让「转换」和「上传」共用同一套
 * 状态与错误提示（缩略图上的菊花、失败后的重试按钮都不用另做一套）。
 */
async function prepareForUpload(item: PendingImage): Promise<File> {
  if (item.prepared) return item.prepared
  if (!isHeic(item.file)) {
    item.prepared = item.file
    return item.file
  }

  const jpeg = await convertHeicToJpeg(item.file)
  if (!jpeg) {
    // 环境限制，不是文件坏了 —— 原样交给后端，让上传失败并给出上面的提示
    item.heicUndecodable = true
    item.prepared = item.file
    return item.file
  }

  item.heicUndecodable = false
  item.prepared = jpeg
  // 顺手把预览换成转好的 JPEG：Chrome 这类解不了 HEIC 的浏览器里，
  // 原来那个 objectURL 会在缩略图位置显示成一张碎图。
  URL.revokeObjectURL(item.previewUrl)
  item.previewUrl = URL.createObjectURL(jpeg)
  return jpeg
}

const placeholder = computed(() => {
  if (!chat.isGenerating) return '给 AI 发消息…'
  // 触屏上 Enter 是换行不是发送，别提 Enter
  return coarsePointer
    ? '正在生成回复…（仍可发送，被丢弃时会提示）'
    : '正在生成回复…（Enter 仍会尝试发送，被丢弃时会提示）'
})

/**
 * 逐张上传（每张一个请求、各自有状态和重试入口）。
 * 失败时把后端 `error.message` 原样留在条目上，发送前会拦下来提示。
 */
async function upload(item: PendingImage): Promise<void> {
  item.status = 'uploading'
  item.error = ''
  const controller = new AbortController()
  item.controller = controller

  const run = (async () => {
    try {
      const prepared = await prepareForUpload(item)
      if (controller.signal.aborted) return
      const list = await uploadAttachments([prepared], { signal: controller.signal })
      const att = list[0]
      if (!att || typeof att.id !== 'number') throw new Error('服务端没有返回附件信息')
      item.attachment = att
      item.status = 'ready'
    } catch (err) {
      // 用户手动移除了这张图 → 请求被 abort，静默收场
      if (err instanceof DOMException && err.name === 'AbortError') return
      item.attachment = null
      item.status = 'error'
      item.error = item.heicUndecodable ? HEIC_HINT : toError(err).message
    } finally {
      if (item.controller === controller) item.controller = null
      item.promise = null
    }
  })()

  item.promise = run
  await run
}

/** 三条上传入口的唯一汇合点 */
function addFiles(files: File[]): void {
  if (files.length === 0) return
  if (disabled.value) {
    toast.info(disabledReason.value)
    return
  }

  const images = files.filter(isImageFile)
  if (images.length === 0) {
    toast.info('只支持图片文件')
    return
  }
  if (images.length < files.length) toast.info('已忽略非图片文件')
  const bigImages = images.filter((f) => f.size > MAX_UPLOAD_BYTES)
  if (bigImages.length > 0) {
    toast.info(`已忽略超过 ${MAX_UPLOAD_MB} MB 的图片：${bigImages.map((f) => f.name).join('、')}`)
  }

  const items: PendingImage[] = images.filter((f) => f.size <= MAX_UPLOAD_BYTES).map((file) => ({
    key: `p${++pendingSeq}`,
    file,
    prepared: null,
    heicUndecodable: false,
    previewUrl: URL.createObjectURL(file),
    status: 'uploading',
    attachment: null,
    error: '',
    promise: null,
    controller: null,
  }))
  // 不做客户端去重：同一张图重复上传后端按 sha256 只存一份、返回同一个 id，
  // 用户看到几张缩略图就发几张，是符合直觉的（契约 §4）。
  //
  // 注意：必须一个个 push 再**从数组里取回响应式代理**去上传。
  // 直接把上面那个原始对象传给 upload() 的话，函数里 `item.status = 'ready'`
  // 改的是原始对象、绕过了 Vue 的 Proxy set 陷阱，界面会永远停在「正在上传」。
  for (const item of items) {
    pending.value.push(item)
    void upload(pending.value[pending.value.length - 1])
  }
}

function retry(item: PendingImage): void {
  if (item.status === 'uploading') return
  void upload(item)
}

function remove(item: PendingImage): void {
  const idx = pending.value.indexOf(item)
  if (idx >= 0) pending.value.splice(idx, 1)
  item.controller?.abort()
  URL.revokeObjectURL(item.previewUrl)
}

function clearImages(): void {
  for (const item of pending.value) {
    item.controller?.abort()
    URL.revokeObjectURL(item.previewUrl)
  }
  pending.value = []
}

/* ---------------- 粘贴 ---------------- */

function onPaste(e: ClipboardEvent): void {
  const dt = e.clipboardData
  if (!dt) return

  const files: File[] = Array.from(dt.files ?? [])
  if (files.length === 0) {
    // 部分浏览器截图粘贴只出现在 items 里，补一遍
    for (const item of Array.from(dt.items ?? [])) {
      if (item.kind !== 'file') continue
      const f = item.getAsFile()
      if (f) files.push(f)
    }
  }
  const images = files.filter(isImageFile)
  if (images.length === 0) return // 普通文本粘贴原样交给 textarea

  // 别让浏览器把图片当文本/文件名插进输入框
  e.preventDefault()
  addFiles(images)
}

/* ---------------- 拖拽 ---------------- */

/** dragenter/dragleave 会在子元素之间反复触发，用计数避免高亮闪烁 */
const dragDepth = ref(0)
const isDragging = computed(() => dragDepth.value > 0)

function hasFiles(e: DragEvent): boolean {
  const types = e.dataTransfer?.types
  if (!types) return false
  return Array.prototype.indexOf.call(types, 'Files') >= 0
}

function onDragEnter(e: DragEvent): void {
  if (!hasFiles(e)) return
  e.preventDefault()
  dragDepth.value += 1
}

function onDragOver(e: DragEvent): void {
  if (!hasFiles(e)) return
  e.preventDefault() // 必须：默认行为是「打开这个文件」，会把页面导航走
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
}

function onDragLeave(e: DragEvent): void {
  if (!hasFiles(e)) return
  dragDepth.value = Math.max(0, dragDepth.value - 1)
}

function onDrop(e: DragEvent): void {
  if (!hasFiles(e)) return
  e.preventDefault() // 同上：阻止浏览器打开文件
  e.stopPropagation()
  dragDepth.value = 0
  addFiles(Array.from(e.dataTransfer?.files ?? []))
}

/** 拖到输入框外面松手时，前面的 dragleave 可能没来，兜底把高亮关掉 */
function resetDrag(): void {
  dragDepth.value = 0
}

/* ---------------- 选文件 ---------------- */

function pickFiles(): void {
  if (disabled.value) {
    toast.info(disabledReason.value)
    return
  }
  fileInput.value?.click()
}

function onFilePicked(e: Event): void {
  const input = e.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = '' // 允许连续选同一个文件
  addFiles(files)
}

/* ---------------- 文本 ---------------- */

function autoGrow(): void {
  const el = area.value
  if (!el) return
  el.style.height = 'auto'
  const next = Math.min(el.scrollHeight, MAX_HEIGHT)
  el.style.height = `${next}px`
  el.style.overflowY = el.scrollHeight > MAX_HEIGHT ? 'auto' : 'hidden'
}

watch(text, () => {
  void nextTick(autoGrow)
})

onMounted(() => {
  autoGrow()
  area.value?.focus()
  // drop 落在输入框外面时 document 上也会收到，用来清掉拖拽高亮
  document.addEventListener('drop', resetDrag)
  document.addEventListener('dragend', resetDrag)
})

onBeforeUnmount(() => {
  document.removeEventListener('drop', resetDrag)
  document.removeEventListener('dragend', resetDrag)
  clearImages()
})

/**
 * 触屏设备（无实体键盘、hover:none）上软键盘的回车只能是裸 Enter，没有
 * Ctrl/Shift 可配 —— 若沿用桌面「裸 Enter 发送」，用户在手机上永远没法换行。
 * 按移动端惯例：触屏 Enter 只换行，发送交给右下角按钮；桌面行为不变。
 * 和 CSS 的触屏断点用同一个条件，保证行为与样式（点击目标放大等）同进同退。
 */
const coarsePointer = window.matchMedia('(hover: none)').matches

function toggleWebSearch(): void {
  if (!chat.conv) return
  void chat.patchConv({ web_search: !chat.conv.web_search })
}

/* ---------------- 会话剩余上下文 ---------------- */

const ctxUsage = computed(() => chat.contextUsage)

/** 紧凑文本：「8.3k/1.0M」；估算值加 ~，宽屏时前面的「上下文」标签由 CSS 控制显隐 */
const ctxText = computed(() => {
  const u = ctxUsage.value
  if (!u) return ''
  return `${u.estimated ? '~' : ''}${abbreviateNumber(u.used)}/${abbreviateNumber(u.total)}`
})

const ctxWarn = computed(() => {
  const u = ctxUsage.value
  return !!u && u.used / u.total > 0.9
})

/** 进度条宽度（0~100） */
const ctxPct = computed(() => {
  const u = ctxUsage.value
  if (!u) return 0
  return Math.min(100, Math.round((u.used / u.total) * 100))
})

/** 详情气泡开关（点击进度条 toggle，点别处收起） */
const ctxInfoOpen = ref(false)

function closeCtxInfo(): void {
  ctxInfoOpen.value = false
}

onMounted(() => document.addEventListener('click', closeCtxInfo))
onBeforeUnmount(() => document.removeEventListener('click', closeCtxInfo))

const ctxTitle = computed(() => {
  const u = ctxUsage.value
  if (!u) return ''
  const pct = Math.round((u.used / u.total) * 100)
  const source = u.estimated
    ? '压缩后的本地估算值，下一轮对话后按上游实测更新'
    : '按上游实测的最近一轮 prompt tokens 计（含 reasoning 回传）'
  return `上下文已用${u.estimated ? '约' : ''} ${formatNumber(u.used)} / ${formatNumber(u.total)}（${pct}%）——${source}；接近上限时建议手动压缩或开新会话`
})

function onKeydown(e: KeyboardEvent): void {
  // 中文输入法守卫（硬性要求）：组字过程中的回车一律放行，不做任何处理
  if (e.isComposing || e.keyCode === 229) return
  if (e.key !== 'Enter') return

  const withModifier = e.ctrlKey || e.metaKey || e.shiftKey || e.altKey
  if (!withModifier) {
    // 触屏：裸 Enter 交给浏览器默认行为（插入换行），发送走按钮
    if (coarsePointer) return
    // 桌面：裸 Enter 发送
    e.preventDefault()
    void submit()
    return
  }

  // Shift+Enter 浏览器默认就会插入换行，交给它即可。
  if (e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) return

  // Ctrl / Cmd / Alt + Enter **不会**触发浏览器的换行默认行为
  // （实测 Chrome：裸 textarea 里 Ctrl+Enter 是空操作，带不带 text 事件都不换行），
  // 所以这里必须自己插，不能指望默认行为。
  e.preventDefault()
  insertNewline()
}

/** 在光标处插入换行，并把光标移到换行之后 */
function insertNewline(): void {
  const el = area.value
  const start = el?.selectionStart ?? text.value.length
  const end = el?.selectionEnd ?? start
  text.value = `${text.value.slice(0, start)}\n${text.value.slice(end)}`
  void nextTick(() => {
    const node = area.value
    if (node) {
      node.selectionStart = node.selectionEnd = start + 1
    }
    autoGrow()
  })
}

/**
 * 发送：
 *   1. 还有图在传 → 先等它们全部落地；
 *   2. 有传失败的图 → 整体不发（文字和图片都留在输入框里，让用户重试或删掉）；
 *   3. POST /api/conversations/{id}/messages，body 带 attachment_ids；
 *   4. 成功 → 清空文字和待发送图；失败（含 409 busy）→ 原样保留。
 */
async function submit(): Promise<void> {
  if (disabled.value || sending.value) return

  const content = text.value.trim()
  const hadImages = pending.value.length > 0
  if (!content && !hadImages && pendingFiles.value.length === 0) return

  sending.value = true
  try {
    // 1. 等上传
    const inflight = pending.value
      .map((p) => p.promise)
      .filter((p): p is Promise<void> => p !== null)
    if (inflight.length > 0) await Promise.all(inflight)

    // 2. 任何一张没传成功就整体不发：用户能看到是哪张、能重试，不用重打一遍字
    const failed = pending.value.filter((p) => p.status !== 'ready' || !p.attachment)
    if (failed.length > 0) {
      const first = failed[0]
      toast.error(
        failed.length === 1
          ? `有 1 张图片没上传成功，先重试或移除它：${first.error || '未知原因'}`
          : `有 ${failed.length} 张图片没上传成功，先重试或移除它们`,
      )
      return
    }

    // 3. 只把后端给的 attachment id 交上去，前端不拼任何图片信息
    const attachmentIds: number[] = []
    for (const p of pending.value) {
      if (p.attachment) attachmentIds.push(p.attachment.id)
    }

    // 3.5 文档同样要先等上传：有在飞的先等，任何一份没传成功就整体不发
    const inflightFiles = pendingFiles.value
      .map((f) => f.promise)
      .filter((p): p is Promise<void> => p !== null)
    if (inflightFiles.length > 0) await Promise.all(inflightFiles)
    const failedFiles = pendingFiles.value.filter((f) => f.status !== 'ready' || f.id <= 0)
    if (failedFiles.length > 0) {
      const first = failedFiles[0]
      toast.error(
        failedFiles.length === 1
          ? `有 1 个文件没上传成功，先重试或移除它：${first.error || '未知原因'}`
          : `有 ${failedFiles.length} 个文件没上传成功，先重试或移除它们`,
      )
      return
    }
    // 文档 id（/api/files 上传时拿到的）
    const fileIds = pendingFiles.value.map((f) => f.id)

    const res = await chat.send(content, attachmentIds, fileIds)
    if (res.ok) {
      // 契约：POST 只返回 ack，内容等 SSE 的 message 事件推下来，这里不做任何插入。
      // 告诉 stream 管家「刚发了一条」：3s 没推到就立即重同步（长连接可能刚静默死掉）
      noteMessageSent()
      // 主动发送后必须回到底部：从搜索跳转/翻历史过来时消息区不在贴底状态，
      // 不发这个请求的话新消息发出去了却停在原地（用户会以为没发出去）
      chat.requestScrollToBottom()
      text.value = ''
      clearDraft()
      clearImages()
      clearPendingFiles()
      await nextTick()
      autoGrow()
      area.value?.focus()
    }
    // res.ok === false：busy 时输入框里的文字和待发送图原样保留（store 已经弹了 toast）
  } finally {
    sending.value = false
  }
}
</script>

<template>
  <div class="composer">
    <div class="composer__inner">
      <div
        class="composer__box"
        :class="{ 'composer__box--disabled': disabled, 'composer__box--drop': isDragging }"
        @dragenter="onDragEnter"
        @dragover="onDragOver"
        @dragleave="onDragLeave"
        @drop="onDrop"
      >
        <!-- 待发送的缩略图：在 textarea 上方，每张右上角可移除 -->
        <div v-if="pending.length > 0" class="composer__atts">
          <div
            v-for="p in pending"
            :key="p.key"
            class="composer__att"
            :class="{ 'composer__att--error': p.status === 'error' }"
            :title="
              p.status === 'error'
                ? `${p.file.name} 上传失败：${p.error}`
                : `${p.file.name}${formatBytes(p.file.size) ? ' · ' + formatBytes(p.file.size) : ''}`
            "
          >
            <!-- 本地 objectURL 预览，不请求服务端、更不请求原图 -->
            <img class="composer__att-img" :src="p.previewUrl" :alt="p.file.name" decoding="async" />

            <span v-if="p.status === 'uploading'" class="composer__att-cover">
              <span class="spinner" />
            </span>
            <button
              v-else-if="p.status === 'error'"
              type="button"
              class="composer__att-cover composer__att-cover--error"
              title="上传失败，点击重试"
              @click="retry(p)"
            >
              <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-alert" />
              </svg>
            </button>

            <button
              type="button"
              class="composer__att-remove"
              :title="p.status === 'uploading' ? '移除（会中断上传）' : '移除这张图'"
              @click="remove(p)"
            >
              <svg width="10" height="10" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-close" />
              </svg>
            </button>
          </div>
        </div>

        <!-- 待发送的文档：小卡片。和图片一致的待遇：上传中有菊花、失败显示原因可重试 -->
        <div v-if="pendingFiles.length > 0" class="composer__atts">
          <div
            v-for="f in pendingFiles"
            :key="f.key"
            class="filechip"
            :class="{ 'filechip--error': f.status === 'error' }"
            :title="
              f.status === 'uploading'
                ? `${f.name} · 上传中…`
                : f.status === 'error'
                  ? `${f.name} 上传失败：${f.error}（点击叹号重试）`
                  : `文件 #${f.id} · ${f.name}`
            "
          >
            <span v-if="f.status === 'uploading'" class="spinner" />
            <svg v-else-if="f.status === 'error'" width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-alert" />
            </svg>
            <svg v-else width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-doc" />
            </svg>
            <span class="filechip__name">{{ f.name }}</span>
            <span class="filechip__size">{{ f.status === 'uploading' ? '上传中…' : formatBytes(f.size) }}</span>
            <button
              v-if="f.status === 'error'"
              type="button"
              class="filechip__remove"
              title="上传失败，点击重试"
              @click="retryPendingFile(f)"
            >
              <svg width="9" height="9" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-refresh" />
              </svg>
            </button>
            <button type="button" class="filechip__remove" title="移除这个文件" @click="removePendingFile(f.key)">
              <svg width="9" height="9" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-close" />
              </svg>
            </button>
          </div>
        </div>

        <!-- 拖动中的视觉反馈：边框高亮 + 这层提示（pointer-events: none，drop 仍落在下面的 box 上） -->
        <div v-if="isDragging" class="composer__drop-hint">松开鼠标即可上传图片</div>

        <textarea
          ref="area"
          v-model="text"
          class="composer__area"
          rows="1"
          spellcheck="false"
          :disabled="disabled"
          :placeholder="placeholder"
          :enterkeyhint="coarsePointer ? 'enter' : 'send'"
          @input="autoGrow"
          @keydown="onKeydown"
          @paste="onPaste"
        />

        <div v-if="disabled" class="composer__notice">
          <span class="composer__notice-text">{{ disabledReason }}</span>
          <button
            v-if="needSettings"
            type="button"
            class="btn btn--sm btn--primary"
            @click="emit('open-settings')"
          >
            打开设置
          </button>
        </div>

        <!-- 第二层：三个工具按钮 + 发送键（右对齐；独占一行，模型名再长也挤不到发送键） -->
        <div class="composer__bar composer__bar--tools">
          <!-- 联网搜索开关：会话级，开了之后模型可在生成中自主调用 web_search。
               只在后端配了搜索后端（settings.webSearchAvailable）且有会话时出现。 -->
          <button
            v-if="settings.webSearchAvailable && chat.conv"
            type="button"
            class="icon-btn composer__search"
            :class="{ 'composer__search--on': chat.conv.web_search }"
            :title="chat.conv.web_search ? '联网搜索：开（模型可自主搜索网页）' : '联网搜索：关'"
            :aria-pressed="chat.conv.web_search"
            @click="toggleWebSearch"
          >
            <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-globe" />
            </svg>
          </button>

          <!-- 图片入口：图片图标；手机上唤起相册 -->
          <button
            type="button"
            class="icon-btn composer__clip"
            :disabled="disabled"
            :title="readyCount > 0 ? `已添加 ${pending.length} 张图片` : '添加图片（也可以直接粘贴或拖进来）'"
            @click="pickFiles"
          >
            <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-image" />
            </svg>
          </button>
          <!-- 这个 input 不能用 hidden / display:none（见 .file-input 的注释）：
               iOS Safari 能正常触发，但 MIUI 浏览器、微信 X5 等 Android WebView
               对不渲染的 file input 根本不弹系统选择器 —— 点了没反应。
               accept 用 image/* 而不是 MIME 清单：部分 Android 机型对多 MIME 的
               intent 解析不到任何应用（表现为没反应或只剩相机）；HEIC 由前端
               转码（addFiles → prepareForUpload），格式最终由后端白名单把关。 -->
          <input
            ref="fileInput"
            type="file"
            class="file-input"
            accept="image/*"
            multiple
            tabindex="-1"
            @change="onFilePicked"
          />

          <!-- 文件入口：回形针（通用「附件」语义）；手机上唤起文件管理器而不是相册。
               accept 见脚本里 docAccept 的注释：Android 下裸 input 会被照片选择器
               接管（只给「照片/拍照」），必须显式给文档类型才走文件管理器；
               iOS 则反过来，accept 会把 .md/.py 这类文件灰掉，保持裸 input。
               同样不能用 hidden / display:none，原因见 .file-input 的注释。 -->
          <button
            type="button"
            class="icon-btn composer__docs"
            :disabled="disabled"
            :title="pendingFiles.length > 0 ? `已添加 ${pendingFiles.length} 个文件` : '添加文件（txt/md/PDF/DOCX/XLSX 等，AI 可用 read_file 读取；不支持的格式会被拒）'"
            @click="pickDocs"
          >
            <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-paperclip" />
            </svg>
          </button>
          <input
            ref="docInput"
            type="file"
            class="file-input"
            multiple
            tabindex="-1"
            :accept="docAccept || undefined"
            @change="onDocPicked"
          />

          <button
            v-if="chat.isGenerating"
            type="button"
            class="composer__send composer__send--stop"
            title="停止生成"
            @click="chat.stop()"
          >
            <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-stop" />
            </svg>
          </button>
          <button
            v-else
            type="button"
            class="composer__send"
            :disabled="!canSubmit"
            :title="coarsePointer ? '发送' : '发送（Enter）'"
            @click="submit"
          >
            <span v-if="sending" class="spinner" />
            <svg v-else width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
              <use href="#i-send" />
            </svg>
          </button>
        </div>

        <!-- 第三层：模型 + 思考强度（右）；状态提示（左） -->
        <div class="composer__bar composer__bar--model">
          <div class="composer__left">
            <span v-if="uploadingCount > 0" class="composer__hint">
              <span class="spinner" /> 正在上传 {{ uploadingCount }} 张图…
            </span>
            <span v-else-if="errorCount > 0" class="composer__hint composer__hint--error">
              {{ errorCount }} 张图没传成功，点击图上的叹号重试
            </span>
            <span v-else class="composer__hint">{{ coarsePointer ? '回车换行 · 点右上按钮发送' : 'Enter 发送 · Ctrl+Enter 换行 · 可粘贴或拖入图片' }}</span>
          </div>

          <ModelPicker />
        </div>

        <!-- 第四层：上下文进度条（整行）：点击就地展开详情气泡，再点/点别处收起 -->
        <div v-if="ctxText" class="composer__bar composer__bar--ctx">
          <button
            type="button"
            class="composer__ctxbar"
            :class="{ 'composer__ctxbar--warn': ctxWarn }"
            :aria-label="ctxTitle"
            :aria-expanded="ctxInfoOpen"
            @click.stop="ctxInfoOpen = !ctxInfoOpen"
          >
            <span class="composer__ctxbar-fill" :style="{ width: ctxPct + '%' }" />
          </button>
          <div v-if="ctxInfoOpen && ctxUsage" class="composer__ctxinfo" @click.stop>
            <div class="composer__ctxinfo-line">
              上下文已用{{ ctxUsage.estimated ? '约' : '' }} <b>{{ formatNumber(ctxUsage.used) }}</b> / {{ formatNumber(ctxUsage.total) }}（{{ ctxPct }}%），剩余约 {{ formatNumber(Math.max(0, ctxUsage.total - ctxUsage.used)) }}
            </div>
            <div class="composer__ctxinfo-hint">
              {{ ctxUsage.estimated ? '压缩后的本地估算值，下一轮对话后按上游实测更新。' : '按上游实测的最近一轮 prompt tokens 计（含 reasoning 回传）。' }}<template v-if="settings.compactThreshold > 0">超过 {{ Math.round(settings.compactThreshold * 100) }}% 建议点右上角按钮手动压缩（旧历史压为摘要并冻结只读）。</template>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
