<script setup lang="ts">
/**
 * 设置弹窗：多供应商（列表 + 就地编辑）与新建会话的默认值。
 *
 * 契约要点（API.md §4）：
 * - 供应商 CRUD：GET/POST /api/providers、PATCH/DELETE /api/providers/{id}
 * - 拉模型 / 测试连接都带 id：POST /api/providers/{id}/models/fetch、/api/providers/{id}/test
 *   —— 结果因此显示在**该供应商那一行**下面，而不是弹窗顶部
 * - POST /api/providers/{id}/test 失败时也返回 HTTP 200 且 ok:false，要把 message 当错误展示
 * - PATCH /api/providers/{id} 里 api_key 省略或空串 = 保留原 key（所以编辑时留空即可）
 * - 默认值：PUT /api/settings/defaults（provider_id / model_id / effort），改动即保存
 * - 思考强度下拉的选项只来自所选模型的 efforts；空数组 = 不支持 → 整个下拉禁用
 */
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import type { Model, Provider } from '@/api/types'
import { EFFORT_LABELS } from '@/api/types'
import { useSettingsStore } from '@/stores/settings'
import { abbreviateNumber, formatNumber } from '@/utils/format'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const settings = useSettingsStore()

/* ------------------------------------------------------------------ */
/* 供应商列表 -> 行视图模型                                             */
/* ------------------------------------------------------------------ */

/** 一行要显示的一切；结果文本在这里算好，模板里不必再做空值收窄 */
interface ProviderRow {
  provider: Provider
  name: string
  models: Model[]
  isDefaultProvider: boolean
  fetchRunning: boolean
  testRunning: boolean
  /** 「获取模型列表」的结果；null = 这一行还没点过 */
  fetchNotice: { ok: boolean; text: string } | null
  /** 「测试连接」的结果；null = 这一行还没点过 */
  testNotice: { ok: boolean; text: string } | null
}

const rows = computed<ProviderRow[]>(() =>
  settings.providers.map((p) => {
    const fetch = settings.fetchResults[p.id] ?? null
    const test = settings.testResults[p.id] ?? null
    return {
      provider: p,
      name: settings.providerName(p.id),
      models: settings.modelsOf(p.id),
      isDefaultProvider: p.id === settings.defaultProviderId,
      fetchRunning: settings.isFetching(p.id),
      testRunning: settings.isTesting(p.id),
      fetchNotice: fetch
        ? {
            ok: fetch.ok,
            // 失败时把后端的 error.message 原样显示，不做任何包装
            text: fetch.ok
              ? `共 ${formatNumber(fetch.total)} 个，新增 ${formatNumber(fetch.added)} 个`
              : fetch.message,
          }
        : null,
      testNotice: test
        ? {
            // 上游报错时接口是 200 + ok:false，message 就是错误说明
            ok: test.ok,
            text: test.ok ? `${test.message} · ${test.latency_ms}ms` : test.message,
          }
        : null,
    }
  }),
)

/* ------------------------------------------------------------------ */
/* 新增 / 编辑表单                                                     */
/* ------------------------------------------------------------------ */

/** 正在编辑的供应商 id（null = 不在编辑） */
const editingId = ref<number | null>(null)
/** 正在新增 */
const adding = ref(false)
/** 正在二次确认删除的供应商 id */
const confirmId = ref<number | null>(null)

const form = reactive({ name: '', base_url: '', api_key: '' })
const formError = ref('')
const formBox = ref<HTMLElement | null>(null)

const formOpen = computed(() => adding.value || editingId.value !== null)
const editingProvider = computed(() =>
  editingId.value === null ? null : settings.providerById(editingId.value),
)
const formTitle = computed(() =>
  adding.value ? '添加供应商' : `编辑供应商 · ${editingProvider.value?.name?.trim() || `#${editingId.value}`}`,
)
/** 编辑时占位符显示该供应商的脱敏 key（留空 = 不修改） */
const keyPlaceholder = computed(() => editingProvider.value?.api_key_hint || 'sk-…')

function resetForm(): void {
  form.name = ''
  form.base_url = ''
  form.api_key = ''
  formError.value = ''
}

async function focusForm(): Promise<void> {
  await nextTick()
  formBox.value?.scrollIntoView({ block: 'nearest' })
}

function startAdd(): void {
  confirmId.value = null
  editingId.value = null
  resetForm()
  adding.value = true
  void focusForm()
}

function startEdit(p: Provider): void {
  confirmId.value = null
  adding.value = false
  editingId.value = p.id
  form.name = p.name
  form.base_url = p.base_url
  form.api_key = '' // 留空 = 不修改现有 key
  formError.value = ''
  void focusForm()
}

function cancelForm(): void {
  adding.value = false
  editingId.value = null
  resetForm()
}

async function submitForm(): Promise<void> {
  formError.value = ''
  const base = form.base_url.trim()
  if (!base) {
    formError.value = 'API 地址不能为空'
    return
  }
  if (!/^https?:\/\//i.test(base)) {
    formError.value = 'API 地址必须以 http:// 或 https:// 开头'
    return
  }
  const name = form.name.trim()
  const key = form.api_key.trim()
  const editing = editingId.value

  const res =
    editing === null
      ? await settings.createProvider({ name, base_url: base, api_key: key })
      : await settings.updateProvider(editing, key ? { name, base_url: base, api_key: key } : { name, base_url: base })

  if (!res.ok) {
    formError.value = res.message
    return
  }
  cancelForm()
}

/* ------------------------------------------------------------------ */
/* 模型表折叠（每家默认收起，页面不再一进来就是一长串）                     */
/* ------------------------------------------------------------------ */

/** 展开了模型表的供应商 id 集合 */
const expandedModels = ref<Set<number>>(new Set())

function toggleModels(id: number): void {
  const s = new Set(expandedModels.value)
  if (s.has(id)) s.delete(id)
  else s.add(id)
  expandedModels.value = s
}

/** 获取模型列表成功就自动展开——点这个按钮的人显然是想看结果 */
async function onFetchModels(id: number): Promise<void> {
  await settings.fetchModelsFromUpstream(id)
  if (settings.fetchResults[id]?.ok && !expandedModels.value.has(id)) {
    expandedModels.value = new Set([...expandedModels.value, id])
  }
}

/* ------------------------------------------------------------------ */
/* 删除（二次确认）                                                     */
/* ------------------------------------------------------------------ */

function askDelete(id: number): void {
  cancelForm()
  confirmId.value = confirmId.value === id ? null : id
}

function doDelete(id: number): void {
  confirmId.value = null
  void settings.removeProvider(id)
}

/* ------------------------------------------------------------------ */
/* 默认值                                                              */
/* ------------------------------------------------------------------ */

function effortText(value: string): string {
  return EFFORT_LABELS[value] ?? value
}

/**
 * <select> / <input type=radio> 的选中状态由浏览器 DOM 自己维护：
 * 保存失败时 store 里的值没变，Vue 就不会重写 DOM，控件会停在一个「假的」选中项上。
 * 保存失败后自增它 → 把这些控件重建一次，DOM 回到 state 的真值。
 */
const controlsEpoch = ref(0)

function isDefaultModel(providerId: number, modelId: string): boolean {
  return settings.defaultProviderId === providerId && settings.defaultModelId === modelId
}

async function onModelRadio(providerId: number, modelId: string): Promise<void> {
  if (isDefaultModel(providerId, modelId)) return
  const res = await settings.chooseDefaultModel(providerId, modelId)
  if (!res.ok) controlsEpoch.value += 1
}

async function onDefaultProviderChange(e: Event): Promise<void> {
  const res = await settings.setDefaultProvider(Number((e.target as HTMLSelectElement).value))
  if (!res.ok) controlsEpoch.value += 1
}

async function onDefaultModelChange(e: Event): Promise<void> {
  const res = await settings.setDefaultModel((e.target as HTMLSelectElement).value)
  if (!res.ok) controlsEpoch.value += 1
}

async function onDefaultEffortChange(e: Event): Promise<void> {
  const res = await settings.setDefaultEffort((e.target as HTMLSelectElement).value)
  if (!res.ok) controlsEpoch.value += 1
}

async function onDefaultSystemPromptChange(e: Event): Promise<void> {
  const res = await settings.saveDefaults({ system_prompt: (e.target as HTMLTextAreaElement).value })
  if (!res.ok) controlsEpoch.value += 1
}

const effortHint = computed(() => {
  if (!settings.defaultModel) return '先选好「默认模型」，思考强度下拉才会有选项。'
  if (!settings.defaultEfforts.length) {
    // 同 ModelPicker：会思考但没档位（MiMo 这类开关式思考）和不支持思考分开说
    return settings.defaultModel.reasoning
      ? '该模型思考为开关式、默认开启，没有强度档位可调。'
      : '该模型不支持思考强度。'
  }
  return '「默认」表示不下发 reasoning_effort 参数，用上游自己的默认行为。'
})

/* ------------------------------------------------------------------ */
/* 生命周期                                                            */
/* ------------------------------------------------------------------ */

watch(
  () => props.open,
  (open) => {
    if (!open) return
    cancelForm()
    confirmId.value = null
    // 上一次的「连接正常 / 共 N 个」不能留到下一次打开
    settings.clearOpResults()
    if (!settings.loaded) void settings.load()
    else void settings.reload()
  },
)

function onDocKeydown(e: KeyboardEvent): void {
  if (!props.open) return
  if (e.key === 'Escape') {
    e.preventDefault()
    if (formOpen.value) cancelForm()
    else emit('close')
  }
}

onMounted(() => document.addEventListener('keydown', onDocKeydown))
onBeforeUnmount(() => document.removeEventListener('keydown', onDocKeydown))
</script>

<template>
  <div v-if="open" class="modal" @click.self="emit('close')">
    <div class="modal__panel" role="dialog" aria-modal="true" aria-label="设置">
      <header class="modal__head">
        <span class="modal__title">设置</span>
        <button type="button" class="icon-btn" title="关闭" @click="emit('close')">
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-close" />
          </svg>
        </button>
      </header>

      <div class="modal__body">
        <!-- ============ 供应商 ============ -->
        <section class="fieldset">
          <div class="fieldset__legend">
            供应商
            <span class="tag tag--muted">{{ settings.providers.length }}</span>
          </div>

          <div v-if="rows.length === 0" class="hint">
            还没有供应商。点下面的「添加供应商」，填上 API 地址（有的上游不需要 Key）即可。
          </div>

          <div v-for="row in rows" :key="row.provider.id" class="prov">
            <div class="prov__head">
              <div class="prov__main">
                <div class="prov__nameline">
                  <span class="prov__name">{{ row.name }}</span>
                  <span v-if="row.isDefaultProvider" class="tag tag--accent">默认</span>
                  <span class="tag tag--muted">{{ row.provider.model_count }} 个模型</span>
                  <span v-if="row.provider.api_key_set" class="tag tag--muted" title="API Key（已脱敏）">
                    {{ row.provider.api_key_hint }}
                  </span>
                  <span v-else class="tag">没有 API Key</span>
                </div>
                <div class="prov__url" :title="row.provider.base_url">
                  {{ row.provider.normalized_base_url || row.provider.base_url }}
                </div>
              </div>

              <div class="prov__actions">
                <button
                  type="button"
                  class="btn btn--sm"
                  :disabled="row.fetchRunning"
                  @click="onFetchModels(row.provider.id)"
                >
                  <span v-if="row.fetchRunning" class="spinner" />
                  获取模型列表
                </button>
                <button
                  type="button"
                  class="btn btn--sm"
                  :disabled="row.testRunning"
                  @click="settings.testConnection(row.provider.id)"
                >
                  <span v-if="row.testRunning" class="spinner" />
                  测试连接
                </button>
                <button type="button" class="icon-btn" title="编辑" @click="startEdit(row.provider)">
                  <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                    <use href="#i-pencil" />
                  </svg>
                </button>
                <button
                  type="button"
                  class="icon-btn icon-btn--danger"
                  title="删除"
                  @click="askDelete(row.provider.id)"
                >
                  <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                    <use href="#i-trash" />
                  </svg>
                </button>
              </div>
            </div>

            <!-- 删除二次确认 -->
            <div v-if="confirmId === row.provider.id" class="conv__confirm prov__confirm">
              <span>删除「{{ row.name }}」？它的模型缓存会一起删掉，引用它的会话会回退到默认供应商。</span>
              <button
                type="button"
                class="btn btn--sm btn--danger"
                :disabled="settings.deletingId === row.provider.id"
                @click="doDelete(row.provider.id)"
              >
                删除
              </button>
              <button type="button" class="btn btn--sm" @click="confirmId = null">取消</button>
            </div>

            <!-- 这一行的操作结果 -->
            <div
              v-if="row.fetchNotice"
              class="notice"
              :class="row.fetchNotice.ok ? 'notice--success' : 'notice--error'"
            >
              <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
                <use :href="row.fetchNotice.ok ? '#i-check' : '#i-alert'" />
              </svg>
              <span class="notice__text">
                <template v-if="row.fetchNotice.ok">获取模型列表：{{ row.fetchNotice.text }}</template>
                <template v-else>获取模型列表失败：{{ row.fetchNotice.text }}</template>
              </span>
            </div>

            <div
              v-if="row.testNotice"
              class="notice"
              :class="row.testNotice.ok ? 'notice--success' : 'notice--error'"
            >
              <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
                <use :href="row.testNotice.ok ? '#i-check' : '#i-alert'" />
              </svg>
              <span class="notice__text">{{ row.testNotice.text }}</span>
            </div>

            <!-- 这家名下的模型：默认折叠，点开关展开 -->
            <button
              v-if="row.models.length"
              type="button"
              class="prov__models-toggle"
              :aria-expanded="expandedModels.has(row.provider.id)"
              @click="toggleModels(row.provider.id)"
            >
              <svg width="11" height="11" viewBox="0 0 16 16" aria-hidden="true" class="prov__models-chevron">
                <use :href="expandedModels.has(row.provider.id) ? '#i-chevron-down' : '#i-chevron-right'" />
              </svg>
              模型列表（{{ row.models.length }}）
            </button>
            <div v-if="row.models.length && expandedModels.has(row.provider.id)" class="prov__models">
              <div class="mtable__wrap">
                <table class="mtable">
                  <thead>
                    <tr>
                      <th>模型</th>
                      <th class="mtable__col-flag">思考档位</th>
                      <th>上下文</th>
                      <th class="mtable__col-radio">默认</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="m in row.models" :key="m.id">
                      <td class="mtable__name" :title="m.display_name || m.model_id">{{ m.model_id }}</td>
                      <td class="mtable__col-flag">
                        <!-- 关心的是档位本身：有档位直接列出来；开关式思考（无档位）和不支持分开说 -->
                        <span v-if="m.efforts?.length" class="tag tag--accent" :title="m.efforts.join(', ')">
                          {{ m.efforts.map(effortText).join('/') }}
                        </span>
                        <span v-else class="tag" :class="m.reasoning ? 'tag--accent' : 'tag--muted'">
                          {{ m.reasoning ? '默认开启' : '不支持' }}
                        </span>
                      </td>
                      <td>{{ m.context_len ? abbreviateNumber(m.context_len) : '—' }}</td>
                      <td class="mtable__col-radio">
                        <label class="radio">
                          <input
                            type="radio"
                            name="default-model"
                            :key="controlsEpoch"
                            :checked="isDefaultModel(row.provider.id, m.model_id)"
                            @change="onModelRadio(row.provider.id, m.model_id)"
                          />
                          默认
                        </label>
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </div>
            <div v-if="!row.models.length" class="hint">这家还没有模型，点上面的「获取模型列表」。</div>
          </div>

          <!-- 新增 / 编辑表单 -->
          <div v-if="formOpen" ref="formBox" class="prov prov--form">
            <div class="fieldset__legend">{{ formTitle }}</div>

            <div class="field">
              <label class="label" for="prov-url">API 地址（Base URL）</label>
              <input
                id="prov-url"
                v-model="form.base_url"
                class="input"
                type="text"
                placeholder="https://api.deepseek.com/v1"
                spellcheck="false"
              />
              <div class="hint">填到 /v1 这一层即可；后端会自动补全常见写法。</div>
            </div>

            <div class="field__row">
              <div class="field">
                <label class="label" for="prov-key">API Key</label>
                <input
                  id="prov-key"
                  v-model="form.api_key"
                  class="input"
                  type="text"
                  autocomplete="off"
                  spellcheck="false"
                  autocapitalize="off"
                  autocorrect="off"
                  data-lpignore="true"
                  data-1p-ignore="true"
                  data-bwignore="true"
                  :placeholder="keyPlaceholder"
                />
              </div>
              <div class="field" style="max-width: 180px">
                <label class="label" for="prov-name">名称</label>
                <input id="prov-name" v-model="form.name" class="input" type="text" placeholder="DeepSeek" />
              </div>
            </div>

            <div class="hint">
              {{
                adding
                  ? '有的上游不需要 Key，可以留空。'
                  : `留空 = 不修改现有 key（当前：${editingProvider?.api_key_set ? editingProvider.api_key_hint : '未配置'}）。`
              }}
            </div>

            <div v-if="formError" class="notice notice--error">
              <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-alert" />
              </svg>
              <span class="notice__text">{{ formError }}</span>
            </div>

            <div class="fieldset__actions">
              <button
                type="button"
                class="btn btn--primary"
                :disabled="settings.savingProvider"
                @click="submitForm"
              >
                <span v-if="settings.savingProvider" class="spinner" />
                保存
              </button>
              <button type="button" class="btn" :disabled="settings.savingProvider" @click="cancelForm">
                取消
              </button>
            </div>
          </div>

          <div v-else class="fieldset__actions">
            <button type="button" class="btn" @click="startAdd">
              <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-plus" />
              </svg>
              添加供应商
            </button>
          </div>
        </section>

        <!-- ============ 默认值（新建会话用） ============ -->
        <section class="fieldset">
          <div class="fieldset__legend">默认值（新建会话用）</div>

          <div v-if="settings.providers.length === 0" class="hint">
            先添加一个供应商，这里才能选默认值。
          </div>

          <template v-else>
            <div class="field__row">
              <div class="field">
                <label class="label" for="default-provider">默认供应商</label>
                <select
                  id="default-provider"
                  class="select select--sm"
                  :key="`p-${controlsEpoch}`"
                  :value="settings.defaultProviderId"
                  :disabled="settings.savingDefaults"
                  @change="onDefaultProviderChange"
                >
                  <option v-for="p in settings.providers" :key="p.id" :value="p.id">
                    {{ settings.providerName(p.id) }}（{{ p.model_count }} 个模型）
                  </option>
                </select>
              </div>

              <div class="field">
                <label class="label" for="default-model">默认模型</label>
                <select
                  id="default-model"
                  class="select select--sm"
                  :key="`m-${controlsEpoch}`"
                  :value="settings.defaultModelId"
                  :disabled="!settings.defaultProviderModels.length || settings.savingDefaults"
                  @change="onDefaultModelChange"
                >
                  <option value="">（未选择）</option>
                  <option v-for="m in settings.defaultProviderModels" :key="m.id" :value="m.model_id">
                    {{ m.model_id }}
                  </option>
                </select>
              </div>
            </div>

            <div class="field">
              <label class="label" for="default-effort">默认思考强度</label>
              <select
                id="default-effort"
                class="select select--sm"
                :key="`e-${controlsEpoch}`"
                :value="settings.defaultModelId ? settings.defaults.effort : ''"
                :disabled="!settings.defaultEfforts.length || settings.savingDefaults"
                @change="onDefaultEffortChange"
              >
                <!-- 固定第一项：值为空串 = 不下发 reasoning_effort -->
                <option value="">默认（不发参数）</option>
                <option v-for="e in settings.defaultEfforts" :key="e" :value="e">
                  {{ effortText(e) }}（{{ e }}）
                </option>
              </select>
              <div class="hint">{{ effortHint }}</div>
            </div>

            <div class="field">
              <label class="label" for="default-sysprompt">默认系统提示词</label>
              <textarea
                id="default-sysprompt"
                class="input"
                rows="3"
                :key="`sp-${controlsEpoch}`"
                :value="settings.defaults.system_prompt"
                :disabled="settings.savingDefaults"
                placeholder="留空 = 不带系统提示词"
                spellcheck="false"
                @change="onDefaultSystemPromptChange"
              />
              <div class="hint">新建会话时自动带上；已有会话不受影响（会话级的在侧栏「⋯」菜单里改）。</div>
            </div>
          </template>
        </section>
      </div>

      <footer class="modal__foot">
        <span class="hint">
          {{ settings.providers.length }} 家供应商 · {{ settings.models.length }} 个模型
        </span>
        <span class="modal__foot-spacer" />
        <span v-if="settings.loadError" class="hint">{{ settings.loadError }}</span>
        <button type="button" class="btn" @click="emit('close')">关闭</button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
/* 多供应商列表比原来的单表单宽，稍微放宽一点面板 */
.modal__panel {
  max-width: 760px;
}

.prov--form {
  gap: var(--sp-3);
  border-color: var(--border-strong);
  background: var(--bg-inset);
}

/* 模型表折叠开关：低调的一行文字按钮，chevron 指示展开态 */
.prov__models-toggle {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 2px 0;
  background: none;
  border: none;
  color: var(--text-muted);
  font-size: var(--fs-sm);
  cursor: pointer;
}

.prov__models-toggle:hover {
  color: var(--text);
}

.prov__models-toggle[aria-expanded='true'] {
  color: var(--text);
}

/* 每家名下的模型表跟着行一起滚，不要在弹窗里再套一个小滚动区 */
.prov__models :deep(.mtable__wrap) {
  max-height: none;
}
</style>
