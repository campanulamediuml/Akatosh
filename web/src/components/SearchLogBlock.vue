<script setup lang="ts">
/**
 * 工具调用展示（联网搜索 / Python 执行 / 生图 / 读文件）。
 *
 * 两种状态：
 * - 生成中（live 非空）：一个实时块，显示「正在搜索：xxx」「正在执行 Python…」
 *   等动作 + 模型正在写的工具参数原文，默认展开，带脉冲点；
 * - 落地后（logJson 非空）：**每次调用一张独立卡片**（时间线一眼看全），
 *   默认折叠；卡片头部带这一段请求各自的 token 数（seg_usages，
 *   把消息的累计成本口径拆成「这一段请求实际多大」，避免「瞬间海量 token」的误读）。
 *
 * 样式骨架复用 .reasoning 那套折叠块（外观与「思考过程」一致），
 * 专有样式在 main.css 的 .searchlog__* / .toolcard。
 */
import { computed, ref, watch } from 'vue'
import type { Usage } from '@/api/types'
import { formatNumber } from '@/utils/format'

interface SearchEntry {
  type?: string // 'search'；历史数据没有 type 字段，缺省视为搜索
  query: string
  results?: { title: string; url: string }[]
  error?: string
}

interface PythonEntry {
  type?: string // 'python'
  code: string
  stdout?: string
  stderr?: string
  exit_code?: number
  error?: string
}

interface ImageEntry {
  type?: string // 'image'
  prompt: string
  error?: string
}

interface FileEntry {
  type?: string // 'file'
  name: string
  id?: number
  pages?: number[]
  images?: boolean
  error?: string
}

type ToolEntry = SearchEntry | PythonEntry | ImageEntry | FileEntry

const props = withDefaults(
  defineProps<{
    /** 生成中的实时动作（chat.searchStatus 里这条消息的条目） */
    live?: string[]
    /** 生成中工具调用的 arguments 流式累积（chat.toolArgs，模型正在写的代码/参数原文） */
    argsText?: string
    /** 消息落库的 search_log（JSON 字符串，搜索与执行记录混排） */
    logJson?: string
    /** 各段流各自的 usage（第 i 次调用对应第 i 段；一条段里多次调用时这几张卡片共享该段） */
    segUsages?: Usage[]
  }>(),
  { live: () => [], argsText: '', logJson: '', segUsages: () => [] },
)

const entries = computed<ToolEntry[]>(() => {
  if (!props.logJson) return []
  try {
    const v = JSON.parse(props.logJson)
    return Array.isArray(v) ? v : []
  } catch {
    return []
  }
})

function isPython(e: ToolEntry): e is PythonEntry {
  return e.type === 'python'
}

function isImage(e: ToolEntry): e is ImageEntry {
  return e.type === 'image'
}

function isFile(e: ToolEntry): e is FileEntry {
  return e.type === 'file'
}

function iconOf(e: ToolEntry): string {
  if (isImage(e)) return '#i-image'
  if (isPython(e)) return '#i-code'
  if (isFile(e)) return '#i-doc'
  return '#i-globe'
}

function labelOf(e: ToolEntry): string {
  if (isImage(e)) return '生成了一张图'
  if (isPython(e)) return '执行了 Python'
  if (isFile(e)) return `读取了文件「${e.name}」`
  return `搜索了「${(e as SearchEntry).query}」`
}

/** 这段请求的 prompt tokens（展示口径：这一段实际多大；累计成本在消息底部） */
function segText(i: number): string {
  const u = props.segUsages[i]
  return u && u.prompt_tokens > 0 ? `↑${formatNumber(u.prompt_tokens)}` : ''
}

const searching = computed(() => props.live.length > 0)
const showLive = computed(() => searching.value)

const liveOpen = ref(true)
// 生成中途开始调用工具时自动展开，让用户看到「它去搜了 / 去跑了」
watch(searching, (v) => {
  if (v) liveOpen.value = true
})

/** 落地卡片各自的展开状态（默认全折叠，不打扰阅读） */
const openCards = ref<Set<number>>(new Set())

function toggleCard(i: number): void {
  const s = new Set(openCards.value)
  if (s.has(i)) s.delete(i)
  else s.add(i)
  openCards.value = s
}

function hostOf(url: string): string {
  try {
    return new URL(url).host
  } catch {
    return url
  }
}
</script>

<template>
  <!-- 生成中：实时块 -->
  <section v-if="showLive" class="reasoning searchlog" :class="{ 'reasoning--open': liveOpen }">
    <button type="button" class="reasoning__head" :aria-expanded="liveOpen" @click="liveOpen = !liveOpen">
      <span class="reasoning__caret" aria-hidden="true">
        <svg width="12" height="12" viewBox="0 0 16 16">
          <use href="#i-chevron-right" />
        </svg>
      </span>
      <svg class="searchlog__icon" width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
        <use href="#i-globe" />
      </svg>
      <span class="reasoning__label">工具调用</span>
      <span class="reasoning__stats">· {{ live.length }} 次</span>
      <span class="reasoning__spacer" />
      <span class="pulse" aria-label="进行中" />
    </button>
    <div v-if="liveOpen" class="reasoning__body searchlog__body">
      <div v-for="(t, i) in live" :key="i" class="searchlog__live">{{ t }}</div>
      <pre v-if="argsText" class="searchlog__args">{{ argsText }}</pre>
    </div>
  </section>

  <!-- 落地：每次调用一张独立卡片 -->
  <div v-else-if="entries.length > 0" class="toolcalls">
    <section
      v-for="(e, i) in entries"
      :key="i"
      class="reasoning toolcard"
      :class="{ 'reasoning--open': openCards.has(i) }"
    >
      <button type="button" class="reasoning__head" :aria-expanded="openCards.has(i)" @click="toggleCard(i)">
        <span class="reasoning__caret" aria-hidden="true">
          <svg width="12" height="12" viewBox="0 0 16 16">
            <use href="#i-chevron-right" />
          </svg>
        </span>
        <svg class="searchlog__icon" width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
          <use :href="iconOf(e)" />
        </svg>
        <span class="reasoning__label">{{ labelOf(e) }}</span>
        <span class="reasoning__spacer" />
        <span v-if="segText(i)" class="reasoning__stats toolcard__seg" :title="'这一段请求的 prompt tokens（消息底部是各段累计的成本口径）'">{{ segText(i) }}</span>
        <span class="reasoning__stats">{{ openCards.has(i) ? '收起' : '展开' }}</span>
      </button>

      <div v-if="openCards.has(i)" class="reasoning__body searchlog__body">
        <div class="searchlog__entry">
          <!-- 文件读取记录（read_file：pybox 提取文本或渲染 PDF 页面） -->
          <template v-if="isFile(e)">
            <div v-if="e.error" class="searchlog__error">{{ e.error }}</div>
            <div v-else class="searchlog__none">
              {{ e.images ? '页面渲染完成' : '提取完成' }}<template v-if="e.pages?.length">（第 {{ e.pages.join('、') }} 页）</template>（内容已发给模型，这里不重复展开）
            </div>
          </template>

          <!-- 图像生成记录（图片本体在消息附件区显示，这里只记 prompt） -->
          <template v-else-if="isImage(e)">
            <div class="searchlog__prompt">「{{ e.prompt }}」</div>
            <div v-if="e.error" class="searchlog__error">{{ e.error }}</div>
          </template>

          <!-- Python 执行记录 -->
          <template v-else-if="isPython(e)">
            <pre class="searchlog__code">{{ e.code }}</pre>
            <div v-if="e.error" class="searchlog__error">{{ e.error }}</div>
            <template v-else>
              <pre v-if="e.stdout" class="searchlog__out">{{ e.stdout }}</pre>
              <pre v-if="e.stderr" class="searchlog__err">{{ e.stderr }}</pre>
              <div v-if="!e.stdout && !e.stderr" class="searchlog__none">（无输出，退出码 {{ e.exit_code ?? 0 }}）</div>
            </template>
          </template>

          <!-- 搜索记录（含无 type 的历史数据） -->
          <template v-else>
            <div v-if="e.error" class="searchlog__error">{{ e.error }}</div>
            <template v-else-if="e.results && e.results.length">
              <a
                v-for="r in e.results"
                :key="r.url"
                class="searchlog__link"
                :href="r.url"
                target="_blank"
                rel="noopener noreferrer"
                :title="r.url"
              >
                <span class="searchlog__host">{{ hostOf(r.url) }}</span>
                <span class="searchlog__title">{{ r.title }}</span>
              </a>
            </template>
            <div v-else class="searchlog__none">没有搜到结果</div>
          </template>
        </div>
      </div>
    </section>
  </div>
</template>
