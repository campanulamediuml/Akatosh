/**
 * markdown-it + highlight.js + DOMPurify。
 * 所有 markdown 渲染都必须经过 renderMarkdown()，不要绕过消毒。
 */

import MarkdownIt from 'markdown-it'
// hljs 按需注册：全量包近 190 种语言（bundle 大头），实际聊天的代码块就这些。
// 没注册的语言会走 highlightAuto（只在已注册集合里检测），不会崩。
import hljs from 'highlight.js/lib/core'
import langBash from 'highlight.js/lib/languages/bash'
import langC from 'highlight.js/lib/languages/c'
import langCpp from 'highlight.js/lib/languages/cpp'
import langCsharp from 'highlight.js/lib/languages/csharp'
import langCss from 'highlight.js/lib/languages/css'
import langDiff from 'highlight.js/lib/languages/diff'
import langGo from 'highlight.js/lib/languages/go'
import langJava from 'highlight.js/lib/languages/java'
import langJavascript from 'highlight.js/lib/languages/javascript'
import langJson from 'highlight.js/lib/languages/json'
import langMarkdown from 'highlight.js/lib/languages/markdown'
import langPlaintext from 'highlight.js/lib/languages/plaintext'
import langPython from 'highlight.js/lib/languages/python'
import langRust from 'highlight.js/lib/languages/rust'
import langSql from 'highlight.js/lib/languages/sql'
import langTypescript from 'highlight.js/lib/languages/typescript'
import langXml from 'highlight.js/lib/languages/xml'
import langYaml from 'highlight.js/lib/languages/yaml'
import DOMPurify from 'dompurify'
import type { Config as PurifyConfig } from 'dompurify'
import mdKatex from '@vscode/markdown-it-katex'
// markdown-it 内部模块：用于下面的 CJK 强调判定补丁（见 md 定义下方的注释）
import StateInline from 'markdown-it/lib/rules_inline/state_inline.mjs'
import { isWhiteSpace, isPunctCharCode, isMdAsciiPunct } from 'markdown-it/lib/common/utils.mjs'

for (const [name, lang] of Object.entries({
  bash: langBash,
  c: langC,
  cpp: langCpp,
  csharp: langCsharp,
  css: langCss,
  diff: langDiff,
  go: langGo,
  java: langJava,
  javascript: langJavascript,
  json: langJson,
  markdown: langMarkdown,
  plaintext: langPlaintext,
  python: langPython,
  rust: langRust,
  sql: langSql,
  typescript: langTypescript,
  xml: langXml, // HTML/SVG 走这个
  yaml: langYaml,
})) {
  hljs.registerLanguage(name, lang)
}

/** hljs 里注册过的语言名集合（用于判断 unknown 语言别乱加 class） */
const KNOWN_LANGS = new Set<string>()

function registerLangs(): void {
  const langs = (hljs as unknown as { listLanguages?: () => string[] }).listLanguages
  if (typeof langs === 'function') {
    for (const name of hljs.listLanguages()) KNOWN_LANGS.add(name)
  }
}

registerLangs()

function escapeHtml(input: string): string {
  return input
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

function highlightCode(code: string, lang: string): string {
  if (lang && KNOWN_LANGS.has(lang)) {
    try {
      return hljs.highlight(code, { language: lang, ignoreIllegals: true }).value
    } catch {
      /* 落到下面自动检测 */
    }
  }
  if (lang && lang !== 'text' && lang !== 'plaintext') {
    try {
      return hljs.highlightAuto(code).value
    } catch {
      /* 落到转义输出 */
    }
  }
  return escapeHtml(code)
}

const md: MarkdownIt = new MarkdownIt({
  // 关掉裸 HTML：内容来自模型，宁可少渲染也不要给它注入面
  html: false,
  linkify: true,
  breaks: true,
  typographer: false,
  langPrefix: 'hljs language-',
  highlight: (code: string, lang: string): string => highlightCode(code, lang),
})

// LaTeX 数学公式（$$..$$ 块级、$..$ 行内）：KaTeX 渲染。
// throwOnError:false —— 模型写出不合法的公式时渲染成红色错误文本而不是炸掉整段消息。
// 行内 $ 的开闭判定是 pandoc 规则（开 $ 后不能跟空格、闭 $ 后不能跟数字），
// 「$5 和 $10」这种价格写法不会被误判成公式。
md.use(mdKatex, { throwOnError: false })

/**
 * 把 \[...\] / \(...\) 分隔符规范化成 $$..$$ / $..$。
 *
 * @vscode/markdown-it-katex 只认 $ 分隔符，但很多模型（尤其中文模型）
 * 习惯输出 \[ \] 和 \( \)。在 md.render 之前统一改写，渲染就走同一套
 * KaTeX 管线。
 *
 * 不能裸 replace：代码块 / 行内代码里的 \[ 是给读者看的字面量，不能动。
 * 所以按行扫，跳过围栏代码块；行内再按 `...` 分段，只改写非代码段。
 * (?<!\\) 防止把 LaTeX 里的换行符 \\[ 误当成公式定界符。
 */
function normalizeTexDelims(source: string): string {
  if (!source.includes('\\[') && !source.includes('\\(')) return source
  const lines = source.split('\n')
  let inFence = false
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    if (/^\s*(`{3,}|~{3,})/.test(line)) {
      inFence = !inFence
      continue
    }
    if (inFence || (!line.includes('\\[') && !line.includes('\\(') && !line.includes('\\]') && !line.includes('\\)')))
      continue
    // 奇数段是行内代码，原样保留
    lines[i] = line
      .split(/(`+[^`]*`+)/)
      .map((seg, idx) =>
        idx % 2 === 1
          ? seg
          : seg
              .replace(/(?<!\\)\\\[/g, '$$')
              .replace(/(?<!\\)\\\]/g, '$$')
              .replace(/(?<!\\)\\\(/g, '$')
              .replace(/(?<!\\)\\\)/g, '$'),
      )
      .join('')
  }
  return lines.join('\n')
}

/* ---------------- CommonMark 的 CJK 强调判定补丁 ----------------
 *
 * 背景：CommonMark 的强调定界符（** __ ~~）能否开/闭，取决于两侧的
 * 「flanking」判定，这套规则是按拉丁文设计的。中文排版里定界符经常
 * 贴着 CJK 文字/标点/引号，三种典型翻车句式（全部来自真实会话）：
 *   1. `**……预备队。**但……`      —— 闭合前面是「。」、后面紧跟汉字
 *   2. `：**"贡献"不是……**`      —— 开启前面是「：」、后面是英文引号
 *   3. `德军约**80%**的伤亡`       —— 闭合前面是 ASCII 标点、后面是汉字
 * 规则全都不允许开/闭 → ** 原样显示。GitHub（cmark-gfm）2024 年为 CJK
 * 修过同样的问题，markdown-it 到 14.3.2 仍未跟进。
 *
 * 第一版补丁把 CJK 标点重分类成「非标点」，结果把句式 2 打破了
 * （开启符两侧都是标点时，规则要求外侧必须是标点才行）——标点类别
 * 不能动，动了就拆东墙补西墙。
 *
 * 正确做法：定界符任一相邻字符是 CJK 文字/标点时，直接放宽 flanking
 * 的标点约束（中文没有词内空格，* 本来就没有拉丁文那种词内歧义）。
 * 拉丁文行为完全不变；`_` 的词内保护（snake_case 不变斜体）也不受
 * 影响 —— 那条路径走的是 canSplitWord=false 分支，见下方断言测试。
 *
 * 实现是 markdown-it@14.3.2 scanDelims 的复制件，仅改动标注
 * 「CJK 补丁」的几行。升级 markdown-it 时应对照上游
 * lib/rules_inline/state_inline.mjs 复查此函数。
 */

/**
 * CJK 文字与语境标点：汉字、假名、谚文、CJK 标点（。、「」【】《》）、
 * 全角形式，以及中文排版标配的弯引号/破折号/省略号。
 * （U+3000 表意空格不在内，它仍是空白。）
 */
function isCjkContext(cc: number): boolean {
  return (
    (cc >= 0x3001 && cc <= 0x303f) || // CJK 标点
    (cc >= 0x3040 && cc <= 0x30ff) || // 平/片假名
    (cc >= 0x3400 && cc <= 0x4dbf) || // CJK 扩展 A
    (cc >= 0x4e00 && cc <= 0x9fff) || // CJK 统一表意文字
    (cc >= 0xac00 && cc <= 0xd7af) || // 谚文音节
    (cc >= 0xf900 && cc <= 0xfaff) || // CJK 兼容表意
    (cc >= 0xff01 && cc <= 0xff65) || // 全角形式
    (cc >= 0x20000 && cc <= 0x2fa1f) || // CJK 扩展 B 及以后
    cc === 0x2018 || // '
    cc === 0x2019 || // '
    cc === 0x201c || // "
    cc === 0x201d || // "
    cc === 0x2013 || // –
    cc === 0x2014 || // —
    cc === 0x2026 // …
  )
}

StateInline.prototype.scanDelims = function (start: number, canSplitWord: boolean) {
  const max = this.posMax
  const marker = this.src.charCodeAt(start)

  // 下方的代理对处理与上游一致：断裂的代理对按 U+FFFD 求值，避免崩溃
  let lastChar: number
  if (start === 0) {
    // 行首视为空白
    lastChar = 0x20
  } else if (start === 1) {
    lastChar = this.src.charCodeAt(0)
    if ((lastChar & 0xf800) === 0xd800) lastChar = 0xfffd
  } else {
    lastChar = this.src.charCodeAt(start - 1)
    if ((lastChar & 0xfc00) === 0xdc00) {
      const highSurr = this.src.charCodeAt(start - 2)
      lastChar = (highSurr & 0xfc00) === 0xd800 ? 0x10000 + ((highSurr - 0xd800) << 10) + (lastChar - 0xdc00) : 0xfffd
    } else if ((lastChar & 0xfc00) === 0xd800) {
      lastChar = 0xfffd
    }
  }

  let pos = start
  while (pos < max && this.src.charCodeAt(pos) === marker) pos++

  const count = pos - start

  // 行尾视为空白
  let nextChar = pos < max ? this.src.charCodeAt(pos) : 0x20
  if ((nextChar & 0xfc00) === 0xd800) {
    const lowSurr = this.src.charCodeAt(pos + 1)
    nextChar = (lowSurr & 0xfc00) === 0xdc00 ? 0x10000 + ((nextChar - 0xd800) << 10) + (lowSurr - 0xdc00) : 0xfffd
  } else if ((nextChar & 0xfc00) === 0xdc00) {
    nextChar = 0xfffd
  }

  // CJK 补丁：标点类别不动；任一相邻字符属于 CJK 语境时放宽 flanking 的标点约束
  const isLastPunctChar = isMdAsciiPunct(lastChar) || isPunctCharCode(lastChar)
  const isNextPunctChar = isMdAsciiPunct(nextChar) || isPunctCharCode(nextChar)
  const cjkAdjacent = isCjkContext(lastChar) || isCjkContext(nextChar)

  const isLastWhiteSpace = isWhiteSpace(lastChar)
  const isNextWhiteSpace = isWhiteSpace(nextChar)

  const left_flanking =
    !isNextWhiteSpace && (!isNextPunctChar || isLastWhiteSpace || isLastPunctChar || cjkAdjacent)
  const right_flanking =
    !isLastWhiteSpace && (!isLastPunctChar || isNextWhiteSpace || isNextPunctChar || cjkAdjacent)

  const can_open = left_flanking && (canSplitWord || !right_flanking || isLastPunctChar)
  const can_close = right_flanking && (canSplitWord || !left_flanking || isNextPunctChar)

  return { can_open, can_close, length: count }
}

/** 语言标签：把 ```js 之类的别名规整成显示用的名字 */
const LANG_ALIAS: Record<string, string> = {
  js: 'javascript',
  ts: 'typescript',
  py: 'python',
  sh: 'bash',
  shell: 'bash',
  yml: 'yaml',
  md: 'markdown',
  rs: 'rust',
  golang: 'go',
}

/** 已知语言的中文/常用显示名 */
const LANG_LABEL: Record<string, string> = {
  javascript: 'JS',
  typescript: 'TS',
  python: 'PY',
  bash: 'SH',
  shell: 'SH',
  json: 'JSON',
  yaml: 'YAML',
  html: 'HTML',
  css: 'CSS',
  go: 'GO',
  rust: 'RS',
  sql: 'SQL',
  java: 'JAVA',
  c: 'C',
  cpp: 'C++',
  csharp: 'C#',
  markdown: 'MD',
  diff: 'DIFF',
  plaintext: 'TEXT',
  text: 'TEXT',
}

export function normalizeLang(raw: string): string {
  const lang = (raw || '').trim().toLowerCase().split(/[\s:{]/)[0] ?? ''
  return LANG_ALIAS[lang] ?? lang
}

export function langLabel(raw: string): string {
  const lang = normalizeLang(raw)
  if (!lang) return 'TEXT'
  return LANG_LABEL[lang] ?? lang.toUpperCase()
}

/**
 * 预处理：markdown-it 默认把 <pre><code> 原样输出，我们这里**不**在 AST 层动手，
 * 而是在渲染后的 DOM 里包一层（见 enhanceCodeBlocks），这样能保留高亮结构。
 */

/** DOMPurify 配置：只放开高亮需要的 class，链接只允许安全协议 */
const PURIFY_CONFIG = {
  // 'style' 是给 KaTeX 的：公式的垂直对齐靠行内 style（strut 高度）。
  // 安全性不受影响 —— markdown-it 的 html:false 保证原始 HTML 进不来，
  // 能带 style 的元素只可能是 KaTeX 自己生成的。
  ALLOWED_ATTR: ['href', 'title', 'target', 'rel', 'class', 'align', 'start', 'colspan', 'rowspan', 'style'],
  ALLOWED_URI_REGEXP: /^(?:https?|mailto|tel):/i,
  ADD_ATTR: ['target', 'rel'],
} as const

/** markdown -> 已消毒的 HTML 字符串 */
export function renderMarkdown(source: string): string {
  if (!source) return ''
  let html = ''
  try {
    html = md.render(normalizeTexDelims(source))
  } catch (err) {
    console.error('[markdown] 渲染失败，降级为纯文本', err)
    return `<p>${escapeHtml(source)}</p>`
  }
  try {
    return DOMPurify.sanitize(html, PURIFY_CONFIG as unknown as PurifyConfig) as unknown as string
  } catch (err) {
    console.error('[markdown] 消毒失败', err)
    return escapeHtml(source)
  }
}

/** 内联片段（标题、提示语）用，不产生 <p> 包裹 */
export function renderInline(source: string): string {
  if (!source) return ''
  try {
    return DOMPurify.sanitize(md.renderInline(normalizeTexDelims(source)), PURIFY_CONFIG as unknown as PurifyConfig) as unknown as string
  } catch {
    return escapeHtml(source)
  }
}

/**
 * 给渲染出来的 HTML 里的每个代码块加上「语言标签 + 复制按钮」的头部。
 * 纯字符串处理，避免为了一个按钮引入 v-html 之外的 DOM 依赖差异。
 * 复制按钮走 MarkdownView 的事件委托。
 */
export function enhanceCodeBlocks(html: string): string {
  if (!html || !html.includes('<pre')) return html

  return html.replace(
    /<pre><code([^>]*)>([\s\S]*?)<\/code><\/pre>/g,
    (_match: string, attrs: string, body: string) => {
      const langMatch = /class="([^"]*?)language-([^"\s]+)/.exec(attrs)
      const rawLang = langMatch?.[2] ?? ''
      const label = langLabel(rawLang)
      const codeText = decodeEntities(stripTags(body))
      const safeText = escapeAttr(codeText)
      return (
        `<div class="code-block">` +
        `<div class="code-block__bar">` +
        `<span class="code-block__lang">${escapeHtml(label)}</span>` +
        `<button type="button" class="code-block__copy" data-copy="${safeText}" aria-label="复制代码">复制</button>` +
        `</div>` +
        `<pre class="code-block__pre"><code${attrs}>${body}</code></pre>` +
        `</div>`
      )
    },
  )
}

function stripTags(html: string): string {
  return html.replace(/<[^>]*>/g, '')
}

function decodeEntities(text: string): string {
  return text
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, '&')
}

function escapeAttr(input: string): string {
  return input
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/\n/g, '&#10;')
}

/** 一步到位：markdown -> 带代码块头部的消毒 HTML */
export function renderMarkdownWithCodeBar(source: string): string {
  return enhanceCodeBlocks(renderMarkdown(source))
}

export default md
